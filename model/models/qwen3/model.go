package qwen3

import (
	"math"

	"github.com/ollama/ollama/fs"
	"github.com/ollama/ollama/kvcache"
	"github.com/ollama/ollama/ml"
	"github.com/ollama/ollama/ml/nn"
	"github.com/ollama/ollama/model"
	"github.com/ollama/ollama/model/input"
)

// NeoX-style rotary position embeddings
const ropeType uint32 = 2

type Options struct {
	hiddenSize, numHeads, numKVHeads int
	eps                              float32
	ropeBase, ropeScale              float32
}

func (o Options) headDim() int {
	return o.hiddenSize / o.numHeads
}

type Attention struct {
	QueryNorm *nn.RMSNorm `gguf:"attn_q_norm"`
	Query     *nn.Linear  `gguf:"attn_q"`
	KeyNorm   *nn.RMSNorm `gguf:"attn_k_norm"`
	Key       *nn.Linear  `gguf:"attn_k"`
	Value     *nn.Linear  `gguf:"attn_v"`
	Output    *nn.Linear  `gguf:"attn_output"`
}

func (sa *Attention) Forward(ctx ml.Context, hiddenStates, positions ml.Tensor, cache kvcache.Cache, opts *Options) ml.Tensor {
	batchSize := hiddenStates.Dim(1)

	query := sa.Query.Forward(ctx, hiddenStates)
	key := sa.Key.Forward(ctx, hiddenStates)
	value := sa.Value.Forward(ctx, hiddenStates)

	query = query.Reshape(ctx, opts.headDim(), opts.numHeads, batchSize)
	key = key.Reshape(ctx, opts.headDim(), opts.numKVHeads, batchSize)
	value = value.Reshape(ctx, opts.headDim(), opts.numKVHeads, batchSize)

	query = sa.QueryNorm.Forward(ctx, query, opts.eps)
	key = sa.KeyNorm.Forward(ctx, key, opts.eps)

	query = query.RoPE(ctx, positions, nil, uint32(opts.headDim()), ropeType, opts.ropeBase, opts.ropeScale)
	key = key.RoPE(ctx, positions, nil, uint32(opts.headDim()), ropeType, opts.ropeBase, opts.ropeScale)

	attention := nn.Attention(ctx, query, key, value, 1./math.Sqrt(float64(opts.headDim())), cache)
	attention = attention.Reshape(ctx, opts.hiddenSize, batchSize)
	return sa.Output.Forward(ctx, attention)
}

type MLP struct {
	Gate *nn.Linear `gguf:"ffn_gate"`
	Up   *nn.Linear `gguf:"ffn_up"`
	Down *nn.Linear `gguf:"ffn_down"`
}

func (mlp *MLP) Forward(ctx ml.Context, hiddenStates ml.Tensor) ml.Tensor {
	hiddenStates = mlp.Gate.Forward(ctx, hiddenStates).SILU(ctx).Mul(ctx, mlp.Up.Forward(ctx, hiddenStates))
	return mlp.Down.Forward(ctx, hiddenStates)
}

type Layer struct {
	AttentionNorm *nn.RMSNorm `gguf:"attn_norm"`
	*Attention

	MLPNorm *nn.RMSNorm `gguf:"ffn_norm"`
	*MLP
}

func (d *Layer) Forward(ctx ml.Context, hiddenStates, positions, outputs ml.Tensor, cache kvcache.Cache, opts *Options) ml.Tensor {
	residual := hiddenStates
	hiddenStates = d.AttentionNorm.Forward(ctx, hiddenStates, opts.eps)
	hiddenStates = d.Attention.Forward(ctx, hiddenStates, positions, cache, opts)

	if outputs != nil {
		hiddenStates = hiddenStates.Rows(ctx, outputs)
		residual = residual.Rows(ctx, outputs)
	}

	hiddenStates = hiddenStates.Add(ctx, residual)

	residual = hiddenStates
	hiddenStates = d.MLPNorm.Forward(ctx, hiddenStates, opts.eps)
	hiddenStates = d.MLP.Forward(ctx, hiddenStates)
	return hiddenStates.Add(ctx, residual)
}

type Model struct {
	model.Base
	model.BytePairEncoding

	TokenEmbedding *nn.Embedding `gguf:"token_embd"`
	OutputNorm     *nn.RMSNorm   `gguf:"output_norm"`
	Output         *nn.Linear    `gguf:"output,alt:token_embd"`

	Layers []Layer `gguf:"blk"`

	*Options
}

// Forward implements model.Model.
func (m *Model) Forward(ctx ml.Context, batch input.Batch) (ml.Tensor, error) {
	positions, err := ctx.Input().FromIntSlice(batch.Positions, len(batch.Positions))
	if err != nil {
		return nil, err
	}

	hiddenStates := m.TokenEmbedding.Forward(ctx, batch.Inputs)

	for i, layer := range m.Layers {
		m.Cache.SetLayer(i)

		var outputs ml.Tensor
		if i == len(m.Layers)-1 {
			outputs, err = ctx.Input().FromIntSlice(batch.Outputs, len(batch.Outputs))
			if err != nil {
				return nil, err
			}
		}

		hiddenStates = layer.Forward(ctx, hiddenStates, positions, outputs, m.Cache, m.Options)
	}

	hiddenStates = m.OutputNorm.Forward(ctx, hiddenStates, m.eps)
	return m.Output.Forward(ctx, hiddenStates), nil
}

func (m *Model) Shift(ctx ml.Context, layer int, key, shift ml.Tensor) (ml.Tensor, error) {
	return key.RoPE(ctx, shift, nil, ropeType, uint32(m.headDim()), m.ropeBase, m.ropeScale), nil
}

var _ model.Model = (*Model)(nil)

func New(c fs.Config) (model.Model, error) {
	m := Model{
		BytePairEncoding: model.NewBytePairEncoding(
			`(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`,
			&model.Vocabulary{
				Values: c.Strings("tokenizer.ggml.tokens"),
				Types:  c.Ints("tokenizer.ggml.token_type"),
				Merges: c.Strings("tokenizer.ggml.merges"),
				BOS:    int32(c.Uint("tokenizer.ggml.bos_token_id")),
				AddBOS: c.Bool("tokenizer.ggml.add_bos_token", true),
				EOS:    int32(c.Uint("tokenizer.ggml.eos_token_id")),
				AddEOS: c.Bool("tokenizer.ggml.add_eos_token", false),
				EOT:    int32(c.Uint("tokenizer.ggml.eos_token_id")),
				AddEOT: c.Bool("tokenizer.ggml.add_eot_token", false),
			},
		),
		Layers: make([]Layer, c.Uint("block_count")),
		Options: &Options{
			hiddenSize: int(c.Uint("embedding_length")),
			numHeads:   int(c.Uint("attention.head_count")),
			numKVHeads: int(c.Uint("attention.head_count_kv")),
			eps:        c.Float("attention.layer_norm_rms_epsilon"),
			ropeBase:   c.Float("rope.freq_base"),
			ropeScale:  c.Float("rope.freq_scale", 1),
		},
	}

	m.Cache = kvcache.NewCausalCache(m.Shift)
	return &m, nil
}

func init() {
	model.Register("qwen3", New)
	model.Register("qwen3moe", New)
}
