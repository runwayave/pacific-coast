package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

type diffSchemaVersionsRequest struct {
	FromVersion int64 `json:"from_version"`
	ToVersion   int64 `json:"to_version"`
}

type diffSchemaVersionsResponse struct {
	FromVersion int64           `json:"from_version"`
	ToVersion   int64           `json:"to_version"`
	Diff        json.RawMessage `json:"diff"`
	FromIR      json.RawMessage `json:"from_ir,omitempty"`
	ToIR        json.RawMessage `json:"to_ir,omitempty"`
}

// cmdDiff — `tide diff <from-version> <to-version>`
//
// Computes the structural diff between two historical schema versions.
// The server loads both IR snapshots and runs ComputeDiff; the CLI
// renders the result.
func cmdDiff(args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: tide diff <from-version> <to-version>")
		return 2
	}
	from, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide diff: invalid from-version: %v\n", err)
		return 2
	}
	to, err := strconv.ParseInt(fs.Arg(1), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide diff: invalid to-version: %v\n", err)
		return 2
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp diffSchemaVersionsResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/DiffSchemaVersions",
		diffSchemaVersionsRequest{FromVersion: from, ToVersion: to}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide diff:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide diff:", err)
			return 3
		}
	case "table":
		printDiffResult(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide diff: unknown --format %q\n", *format)
		return 3
	}
	return 0
}

type diffChange struct {
	Kind     string `json:"kind"`
	EntityID string `json:"entity_id"`
	Field    string `json:"field,omitempty"`
	Detail   string `json:"detail,omitempty"`
	From     any    `json:"from,omitempty"`
	To       any    `json:"to,omitempty"`
}

type irSchema struct {
	Entities []irEntity `json:"entities"`
}

type irEntity struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Fields    []irField `json:"fields"`
	Table     string    `json:"table,omitempty"`
}

func (e irEntity) ID() string { return e.Namespace + "." + e.Name }

type irField struct {
	Name    string      `json:"name"`
	Type    irFieldType `json:"type"`
	NotNull bool        `json:"not_null,omitempty"`
	Primary bool        `json:"primary,omitempty"`
	Serial  bool        `json:"serial,omitempty"`
	Unique  bool        `json:"unique,omitempty"`
	Default interface{} `json:"default,omitempty"`
	Ref     interface{} `json:"ref,omitempty"`
}

type irFieldType struct {
	Name  string       `json:"name"`
	Len   int          `json:"len,omitempty"`
	Array bool         `json:"array,omitempty"`
	Elem  *irFieldType `json:"elem,omitempty"`
}

func renderType(t irFieldType) string {
	if t.Array && t.Elem != nil {
		return "[]" + renderType(*t.Elem)
	}
	if t.Len > 0 {
		return fmt.Sprintf("%s(%d)", t.Name, t.Len)
	}
	return t.Name
}

func renderFieldLine(f irField) string {
	typStr := renderType(f.Type)
	mods := ""
	if f.Primary {
		mods += " primary"
	}
	if f.Serial {
		mods += " serial"
	}
	if f.NotNull {
		mods += " not null"
	}
	if f.Unique {
		mods += " unique"
	}
	return fmt.Sprintf("%-26s %s%s", f.Name, typStr, mods)
}

func printDiffResult(resp diffSchemaVersionsResponse) {
	fmt.Printf("%s %s %s %s\n\n",
		cliout.Bold("diff"),
		cliout.Red(fmt.Sprintf("v%d", resp.FromVersion)),
		cliout.Grey("→"),
		cliout.Green(fmt.Sprintf("v%d", resp.ToVersion)))

	var d struct {
		Additive         []diffChange `json:"additive"`
		BackfillRequired []diffChange `json:"backfill_required"`
		Breaking         []diffChange `json:"breaking"`
	}
	if err := json.Unmarshal(resp.Diff, &d); err != nil {
		fmt.Println(string(resp.Diff))
		return
	}

	all := append(append(d.Additive, d.BackfillRequired...), d.Breaking...)
	if len(all) == 0 {
		fmt.Println(cliout.Grey("(no changes)"))
		return
	}

	var fromIR, toIR irSchema
	_ = json.Unmarshal(resp.FromIR, &fromIR)
	_ = json.Unmarshal(resp.ToIR, &toIR)

	fromEntities := map[string]*irEntity{}
	for i := range fromIR.Entities {
		e := &fromIR.Entities[i]
		fromEntities[e.ID()] = e
	}
	toEntities := map[string]*irEntity{}
	for i := range toIR.Entities {
		e := &toIR.Entities[i]
		toEntities[e.ID()] = e
	}

	byEntity := map[string][]diffChange{}
	var entityOrder []string
	for _, ch := range all {
		if _, seen := byEntity[ch.EntityID]; !seen {
			entityOrder = append(entityOrder, ch.EntityID)
		}
		byEntity[ch.EntityID] = append(byEntity[ch.EntityID], ch)
	}

	for _, eid := range entityOrder {
		changes := byEntity[eid]
		fromEnt := fromEntities[eid]
		toEnt := toEntities[eid]

		suffix := ""
		if fromEnt == nil && toEnt != nil {
			suffix = " " + cliout.Green("(new entity)")
		} else if fromEnt != nil && toEnt == nil {
			suffix = " " + cliout.Red("(removed)")
		}
		fmt.Printf("%s %s%s\n", cliout.Bold(cliout.Cyan("───")), cliout.Bold(cliout.Cyan(eid)), suffix)

		changedFields := map[string]bool{}
		for _, ch := range changes {
			if ch.Field != "" {
				changedFields[ch.Field] = true
			}
		}

		if toEnt != nil {
			fromFields := map[string]irField{}
			if fromEnt != nil {
				for _, f := range fromEnt.Fields {
					fromFields[f.Name] = f
				}
			}

			for _, f := range toEnt.Fields {
				line := renderFieldLine(f)
				if _, wasInOld := fromFields[f.Name]; !wasInOld {
					fmt.Printf("  %s %s\n", cliout.Green("+"), cliout.Green(line))
				} else if changedFields[f.Name] {
					oldLine := renderFieldLine(fromFields[f.Name])
					fmt.Printf("  %s %s\n", cliout.Red("-"), cliout.Red(oldLine))
					fmt.Printf("  %s %s\n", cliout.Green("+"), cliout.Green(line))
				} else {
					fmt.Printf("    %s\n", cliout.Grey(line))
				}
			}

			if fromEnt != nil {
				toFields := map[string]bool{}
				for _, f := range toEnt.Fields {
					toFields[f.Name] = true
				}
				for _, f := range fromEnt.Fields {
					if !toFields[f.Name] {
						fmt.Printf("  %s %s\n", cliout.Red("-"), cliout.Red(renderFieldLine(f)))
					}
				}
			}
		} else if fromEnt != nil {
			for _, f := range fromEnt.Fields {
				fmt.Printf("  %s %s\n", cliout.Red("-"), cliout.Red(renderFieldLine(f)))
			}
		}
		fmt.Println()
	}

	parts := []string{}
	if len(d.Additive) > 0 {
		parts = append(parts, cliout.Green(fmt.Sprintf("+%d additive", len(d.Additive))))
	}
	if len(d.BackfillRequired) > 0 {
		parts = append(parts, cliout.Yellow(fmt.Sprintf("~%d backfill", len(d.BackfillRequired))))
	}
	if len(d.Breaking) > 0 {
		parts = append(parts, cliout.Red(fmt.Sprintf("!%d breaking", len(d.Breaking))))
	}
	fmt.Println(strings.Join(parts, "  "))
}
