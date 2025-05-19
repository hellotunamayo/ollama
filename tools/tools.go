package tools

import (
	"errors"
	"strings"
	gotmpl "text/template"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/template"
)

// Sentinel errors for parsing states
var (
	ErrPartialPrefix = errors.New("partial prefix detected")

	ErrPrefixNotFound = errors.New("prefix not found")

	ErrInvalidToolCall = errors.New("invalid tool call format")

	ErrAccumulateMore = errors.New("need to accumulate more content")
)

type Parser struct {
	greedyParse bool
	prefixFound bool
	tmpl        gotmpl.Template
	sb          strings.Builder
	prefix      string
	index       int
	name        string
	arguments   string
	Done        bool
}

// checkPrefix processes a string to find and handle a prefix pattern.
//
// Returns:
//   - The processed string with prefix removed if found
//   - error: ErrPartialPrefix if prefix is incomplete, ErrPrefixNotFound if not found, or nil if successful
func (p *Parser) checkPrefix(s string) (string, error) {
	// Keep original for overlap checks
	original := s

	if s == "" {
		return "", nil
	}

	// If no prefix defined, just return trimmed string
	if p.prefix == "" {
		return s, nil
	}

	// Check for prefix at start of string
	if processedStr, hasPrefix := strings.CutPrefix(s, p.prefix); hasPrefix {
		// Found prefix at start - accumulate for potential tool
		p.prefixFound = true
		return processedStr, nil
	}

	// Check if prefix overlaps end of string
	if overlap := suffixOverlap(original, p.prefix); overlap > 0 {
		// Return everything except overlapping portion
		p.sb.Reset()
		p.sb.WriteString(original[len(original)-overlap:])
		return original[0 : len(original)-overlap], ErrAccumulateMore
	}

	// Check if prefix appears in middle of string
	if idx := strings.Index(original, p.prefix); idx != -1 {
		// Save remainder starting at prefix for next pass
		p.sb.Reset()
		p.sb.WriteString(strings.TrimSpace(original[idx:]))
		// Return everything before prefix
		return original[:idx], ErrAccumulateMore
	}

	// No partial prefix found
	return s, nil
}

// Add processes a string input to parse tool calls and content.
// It handles prefix detection and JSON parsing to extract tool calls.
//
// Returns:
//   - tools: Any parsed tool calls
//   - content: Non-tool call content
//   - error: One of the sentinel errors or nil if successful
func (p *Parser) Add(s string) (tools []api.ToolCall, content string, err error) {
	if s == "" {
		return nil, "", ErrAccumulateMore
	}
	p.sb.WriteString(s)
	s = p.sb.String()

	// Check for prefix pattern in input
	s, err = p.checkPrefix(s)
	if err != nil {
		if s != "" {
			// Return content before prefix
			return nil, s, nil
		}
		// Need more input to complete prefix
		return nil, "", ErrAccumulateMore
	}

	// Exit if prefix exists in template, greedy parsing is off, and prefix not found
	if !p.greedyParse && !p.prefixFound {
		p.sb.Reset()
		return nil, "", ErrPrefixNotFound
	}

	toolCalls, err := parseJSONToolCalls(s, p.name, p.arguments)
	if err != nil {
		if errors.Is(err, ErrAccumulateMore) {
			return nil, "", err
		} else {
			p.sb.Reset()
			// Do not try greedy parsing if JSON not found
			p.greedyParse = false
			if p.prefix == "" {
				p.Done = true
			}
			if p.prefixFound {
				// Drop tokens since prefix was found
				return nil, "", ErrAccumulateMore
			}
			return nil, s, nil
		}
	}

	for _, tc := range toolCalls {
		tc.Function.Index = p.index
		p.index++
	}

	// Mark as done if no prefix needed
	if p.prefix == "" {
		p.Done = true
	}

	p.sb.Reset()
	return toolCalls, "", nil
}

// NewParser creates a new tool call parser from a template. It extracts the tool call format,
// prefix, and field names from the template to use for parsing tool calls from model output.
//
// Returns an error if the template does not contain valid tool call formatting.
func NewParser(templateToProcess *gotmpl.Template) (Parser, error) {
	parsed, err := template.Parse(templateToProcess.Root.String())
	if err != nil {
		return Parser{}, err
	}

	tt, err := toolTemplate(parsed)
	if err != nil {
		return Parser{}, err
	}

	tp := toolPrefix(templateToProcess)
	tp = strings.TrimSpace(tp)

	name, arguments, err := extractToolArgs(tt)
	if err != nil {
		return Parser{}, err
	}

	return Parser{
		tmpl:        *tt,
		sb:          strings.Builder{},
		prefix:      tp,
		greedyParse: true,
		name:        name,
		arguments:   arguments,
	}, nil
}
