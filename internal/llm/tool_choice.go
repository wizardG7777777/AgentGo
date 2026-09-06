package llm

import (
	"fmt"
	"strings"
)

type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceFunction ToolChoiceMode = "function"
)

type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode,omitempty"`
	Name string         `json:"name,omitempty"`
}

func (c ToolChoice) Validate() error {
	switch c.Mode {
	case "", ToolChoiceAuto:
		if c.Name != "" {
			return fmt.Errorf("tool_choice auto 不得指定 name")
		}
	case ToolChoiceRequired:
		if c.Name != "" {
			return fmt.Errorf("tool_choice required 不得指定 name")
		}
	case ToolChoiceFunction:
		if strings.TrimSpace(c.Name) == "" || len([]rune(c.Name)) > 128 {
			return fmt.Errorf("tool_choice function name 无效")
		}
		for _, r := range c.Name {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return fmt.Errorf("tool_choice function name=%q 含非法字符", c.Name)
			}
		}
	default:
		return fmt.Errorf("tool_choice mode=%q 无效", c.Mode)
	}
	return nil
}
