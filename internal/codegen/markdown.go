package codegen

import (
	"fmt"
	"strings"

	"github.com/soroworks/sorovault/internal/model"
)

// Markdown renders a contract interface as a standalone Markdown reference.
func Markdown(iface *model.Interface, src Source) (string, error) {
	if iface == nil {
		return "", fmt.Errorf("codegen: cannot document a nil interface")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Contract `%s`\n\n", src.ContractID)
	fmt.Fprintf(&b, "Network: `%s`  \nWASM hash: `%s`\n", src.Network, src.WasmHash)
	if iface.Meta != nil {
		if version := iface.Meta["rssdkver"]; version != "" {
			fmt.Fprintf(&b, "  \nBuilt with soroban-sdk `%s`", oneLine(version))
		}
	}
	b.WriteString("\n")

	b.WriteString("\n## Functions\n")
	if len(iface.Functions) == 0 {
		b.WriteString("\nNo functions.\n")
	}
	for _, fn := range iface.Functions {
		fmt.Fprintf(&b, "\n### `%s`\n", fn.Signature())
		writeMarkdownDoc(&b, fn.Doc)
		for _, in := range fn.Inputs {
			fmt.Fprintf(&b, "\n- `%s: %s`", in.Name, model.Render(in.Type))
			if in.Doc != "" {
				fmt.Fprintf(&b, " - %s", oneLine(in.Doc))
			}
		}
		if len(fn.Inputs) > 0 {
			b.WriteString("\n")
		}
		if len(fn.Outputs) > 0 {
			fmt.Fprintf(&b, "\nReturns: `%s`\n", model.Render(fn.Outputs[0]))
		}
	}

	t := iface.Types
	if len(t.Structs)+len(t.Unions)+len(t.Enums)+len(t.ErrorEnums) > 0 {
		b.WriteString("\n## Types\n")
		for _, s := range t.Structs {
			kind := "struct"
			if s.IsTuple {
				kind = "tuple struct"
			}
			fmt.Fprintf(&b, "\n### `%s %s`\n", kind, s.Name)
			writeMarkdownDoc(&b, s.Doc)
			for _, field := range s.Fields {
				fmt.Fprintf(&b, "\n- `%s: %s`", field.Name, model.Render(field.Type))
				if field.Doc != "" {
					fmt.Fprintf(&b, " - %s", oneLine(field.Doc))
				}
			}
			if len(s.Fields) > 0 {
				b.WriteString("\n")
			}
		}
		for _, u := range t.Unions {
			fmt.Fprintf(&b, "\n### `enum %s`\n", u.Name)
			writeMarkdownDoc(&b, u.Doc)
			for _, c := range u.Cases {
				fmt.Fprintf(&b, "\n- `%s`", c.Name)
				if len(c.Values) > 0 {
					values := make([]string, len(c.Values))
					for i, value := range c.Values {
						values[i] = model.Render(value)
					}
					fmt.Fprintf(&b, "(%s)", strings.Join(values, ", "))
				}
				if c.Doc != "" {
					fmt.Fprintf(&b, " - %s", oneLine(c.Doc))
				}
			}
			if len(u.Cases) > 0 {
				b.WriteString("\n")
			}
		}
		for _, e := range t.Enums {
			fmt.Fprintf(&b, "\n### `enum %s`\n", e.Name)
			writeMarkdownDoc(&b, e.Doc)
			for _, c := range e.Cases {
				fmt.Fprintf(&b, "\n- `%s = %d`", c.Name, c.Value)
				writeMarkdownDocInline(&b, c.Doc)
			}
			if len(e.Cases) > 0 {
				b.WriteString("\n")
			}
		}
		for _, e := range t.ErrorEnums {
			fmt.Fprintf(&b, "\n### `error %s`\n", e.Name)
			writeMarkdownDoc(&b, e.Doc)
			for _, c := range e.Cases {
				fmt.Fprintf(&b, "\n- `%s = %d`", c.Name, c.Value)
				writeMarkdownDocInline(&b, c.Doc)
			}
			if len(e.Cases) > 0 {
				b.WriteString("\n")
			}
		}
	}

	if len(iface.Events) > 0 {
		b.WriteString("\n## Events\n")
		for _, event := range iface.Events {
			fmt.Fprintf(&b, "\n### `%s`\n", event.Name)
			writeMarkdownDoc(&b, event.Doc)
			if len(event.Prefix) > 0 {
				fmt.Fprintf(&b, "\nTopics: `%s`\n", strings.Join(event.Prefix, "`, `"))
			}
			for _, param := range event.Params {
				fmt.Fprintf(&b, "\n- `%s: %s` (%s)", param.Name, model.Render(param.Type), param.Location)
				if param.Doc != "" {
					fmt.Fprintf(&b, " - %s", oneLine(param.Doc))
				}
			}
			if len(event.Params) > 0 {
				b.WriteString("\n")
			}
			if event.DataFormat != "" {
				fmt.Fprintf(&b, "\nData format: `%s`\n", event.DataFormat)
			}
		}
	}

	return b.String(), nil
}

func writeMarkdownDoc(b *strings.Builder, doc string) {
	if doc == "" {
		return
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(doc))
	b.WriteString("\n")
}

func writeMarkdownDocInline(b *strings.Builder, doc string) {
	if doc != "" {
		fmt.Fprintf(b, " - %s", oneLine(doc))
	}
}
