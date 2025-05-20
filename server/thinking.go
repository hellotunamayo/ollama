package server

import (
	"strings"
	"text/template"
	"text/template/parse"
	"unicode"
)

type thinkingParseState int

const (
	// We're looking for the opening tag, but we haven't seen any non-whitespace
	// characters yet
	thinkingParseState_LookingForOpening thinkingParseState = iota
	// We've seen the opening tag, but we haven't seen the closing tag yet
	thinkingParseState_Thinking
	// We've seen the closing tag, but we haven't seen any non-whitespace
	// characters after the closing tag yet (we want to eat any whitespace between
	// the closing tag and the content)
	thinkingParseState_ThinkingDoneEatingWhitespace
	// We've seen the closing tag and seen at least one non-whitespace character
	// after it
	thinkingParseState_ThinkingDone
)

func (s thinkingParseState) String() string {
	switch s {
	case thinkingParseState_LookingForOpening:
		return "LookingForOpening"
	case thinkingParseState_Thinking:
		return "Thinking"
	case thinkingParseState_ThinkingDoneEatingWhitespace:
		return "ThinkingDoneEatingWhitespace"
	case thinkingParseState_ThinkingDone:
		return "ThinkingDone"
	default:
		return "Unknown"
	}
}

type thinkingParser struct {
	state      thinkingParseState
	openingTag string
	closingTag string
	acc        strings.Builder
}

// returns the thinking content and the normal content that should be
// immediately sent to the user. It will internally buffer if it needs to see
// more content to disambiguate
func (s *thinkingParser) addContent(content string) (string, string) {
	s.acc.WriteString(content)

	var thinkingAcc, remainingAcc strings.Builder

	var thinking, remaining string
	keepLooping := true
	// we loop because we might pass through multiple parsing states in a single
	// call to addContent, and we want to make sure callers don't have to wait for
	// data that's already unambiguous
	for keepLooping {
		thinking, remaining, keepLooping = eat(s)
		thinkingAcc.WriteString(thinking)
		remainingAcc.WriteString(remaining)
	}

	return thinkingAcc.String(), remainingAcc.String()
}

// the additional bool return is true iff we should continue eating
func eat(s *thinkingParser) (string, string, bool) {
	switch s.state {
	case thinkingParseState_LookingForOpening:
		trimmed := strings.TrimLeftFunc(s.acc.String(), unicode.IsSpace)
		if strings.HasPrefix(trimmed, s.openingTag) {
			after := strings.Join(strings.Split(trimmed, s.openingTag)[1:], s.openingTag)
			after = strings.TrimLeftFunc(after, unicode.IsSpace)
			// after might contain more than just thinking tokens, so we continue
			// parsing instead of returning it as thinking tokens here
			s.acc.Reset()
			s.acc.WriteString(after)
			s.state = thinkingParseState_Thinking
			return "", "", true
		} else if strings.HasPrefix(s.openingTag, trimmed) {
			// partial opening seen, so let's keep accumulating
			return "", "", false
		} else if trimmed == "" {
			// saw whitespace only, so let's keep accumulating
			return "", "", false
		} else {
			// didn't see an opening tag, but we have content, so thinking was skipped
			s.state = thinkingParseState_ThinkingDone
			// note that we use the original content, not the trimmed one because we
			// don't want to eat any whitespace in the real content if there were no
			// thinking tags
			return "", s.acc.String(), false
		}
	case thinkingParseState_Thinking:
		acc := s.acc.String()
		if strings.Contains(acc, s.closingTag) {
			split := strings.Split(acc, s.closingTag)
			thinking := split[0]
			remaining := strings.Join(split[1:], s.closingTag)
			remaining = strings.TrimLeftFunc(remaining, unicode.IsSpace)
			s.acc.Reset()
			if remaining == "" {
				s.state = thinkingParseState_ThinkingDoneEatingWhitespace
			} else {
				s.state = thinkingParseState_ThinkingDone
			}
			return thinking, remaining, false
		} else if overlapLen := overlap(acc, s.closingTag); overlapLen > 0 {
			thinking := acc[:len(acc)-overlapLen]
			remaining := acc[len(acc)-overlapLen:]
			s.acc.Reset()
			// keep track of the candidate closing tag. We have to buffer it until it
			// becomes disambiguated
			s.acc.WriteString(remaining)
			return thinking, "", false
		} else {
			// purely just thinking tokens, so we can return them
			s.acc.Reset()
			return acc, "", false
		}
	case thinkingParseState_ThinkingDoneEatingWhitespace:
		trimmed := strings.TrimLeftFunc(s.acc.String(), unicode.IsSpace)
		s.acc.Reset()
		// if we see non-whitespace, we're done eating the leading whitespace of the content
		if trimmed != "" {
			s.state = thinkingParseState_ThinkingDone
		}
		return "", trimmed, false
	case thinkingParseState_ThinkingDone:
		acc := s.acc.String()
		s.acc.Reset()
		return "", acc, false
	default:
		panic("unknown state")
	}
}

// longest overlap between suffix of s and prefix of delim
func overlap(s, delim string) int {
	max := min(len(delim), len(s))
	for i := max; i > 0; i-- {
		if strings.HasSuffix(s, delim[:i]) {
			return i
		}
	}
	return 0
}

func templateVisit(n parse.Node, enterFn func(parse.Node) bool, exitFn func(parse.Node)) {
	if n == nil {
		return
	}
	shouldContinue := enterFn(n)
	if !shouldContinue {
		return
	}
	switch x := n.(type) {
	case *parse.ListNode:
		for _, c := range x.Nodes {
			templateVisit(c, enterFn, exitFn)
		}
	case *parse.BranchNode:
		if x.Pipe != nil {
			templateVisit(x.Pipe, enterFn, exitFn)
		}
		if x.List != nil {
			templateVisit(x.List, enterFn, exitFn)
		}
		if x.ElseList != nil {
			templateVisit(x.ElseList, enterFn, exitFn)
		}
	case *parse.ActionNode:
		templateVisit(x.Pipe, enterFn, exitFn)
	case *parse.WithNode:
		templateVisit(&x.BranchNode, enterFn, exitFn)
	case *parse.RangeNode:
		templateVisit(&x.BranchNode, enterFn, exitFn)
	case *parse.IfNode:
		templateVisit(&x.BranchNode, enterFn, exitFn)
	case *parse.TemplateNode:
		templateVisit(x.Pipe, enterFn, exitFn)
	case *parse.PipeNode:
		for _, c := range x.Cmds {
			templateVisit(c, enterFn, exitFn)
		}
	case *parse.CommandNode:
		for _, a := range x.Args {
			templateVisit(a, enterFn, exitFn)
		}
		// text, field, number, etc. are leaves – nothing to recurse into
	}
	if exitFn != nil {
		exitFn(n)
	}
}

// We use a heuristic to infer the tags that surround thinking traces:
// We look for a range node that iterates over "Messages" and then look for a
// reference to "Thinking" like `{{.Thinking}}`. We then go up to the nearest
// ListNode and take the first and last TextNodes as the opening and closing
// tags.
func inferThinkingTags(t *template.Template) (string, string) {
	ancestors := []parse.Node{}

	openingTag := ""
	closingTag := ""

	enterFn := func(n parse.Node) bool {
		ancestors = append(ancestors, n)

		switch x := n.(type) {
		case *parse.FieldNode:
			if len(x.Ident) > 0 && x.Ident[0] == "Thinking" {
				var mostRecentRange *parse.RangeNode
				for i := len(ancestors) - 1; i >= 0; i-- {
					if r, ok := ancestors[i].(*parse.RangeNode); ok {
						mostRecentRange = r
						break
					}
				}
				if mostRecentRange == nil || !rangeUsesField(mostRecentRange, "Messages") {
					return true
				}

				// TODO(drifkin): to be more robust, check that it's in the action
				// part, not the `if`'s pipeline part. We do match on the nearest list
				// that starts and ends with text nodes, which makes this not strictly
				// necessary for our heuristic

				// go up to the nearest ancestor that is a *parse.ListNode
				for i := len(ancestors) - 1; i >= 0; i-- {
					if l, ok := ancestors[i].(*parse.ListNode); ok {
						firstNode := l.Nodes[0]
						if t, ok := firstNode.(*parse.TextNode); ok {
							openingTag = strings.TrimSpace(t.String())
						}
						lastNode := l.Nodes[len(l.Nodes)-1]
						if t, ok := lastNode.(*parse.TextNode); ok {
							closingTag = strings.TrimSpace(t.String())
						}

						break
					}
				}
			}
		}

		return true
	}

	exitFn := func(n parse.Node) {
		ancestors = ancestors[:len(ancestors)-1]
	}

	templateVisit(t.Root, enterFn, exitFn)

	return openingTag, closingTag
}

// checks to see if the given field name is present in the pipeline of the given range node
func rangeUsesField(rangeNode *parse.RangeNode, field string) bool {
	found := false
	enterFn := func(n parse.Node) bool {
		switch x := n.(type) {
		case *parse.FieldNode:
			if x.Ident[0] == field {
				found = true
			}
		}
		return true
	}
	templateVisit(rangeNode.BranchNode.Pipe, enterFn, nil)
	return found
}
