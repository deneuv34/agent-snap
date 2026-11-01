package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/deneuv34/agent-snap/internal/protocol"
	"github.com/deneuv34/agent-snap/internal/snapshot"
)

const usage = `agent-snap - zero-impact working-tree snapshots for agent sessions

Usage:
  agent-snap create [reason] [--cwd <dir>]     create a snapshot, print JSON
  agent-snap list [--cwd <dir>] [--limit <n>]  list snapshots, print JSON
  agent-snap get <id> [--cwd <dir>]            show one snapshot, print JSON
  agent-snap diff <from> <to> [--cwd <dir>] [--offset <n>] [--max-patch-bytes <n>]
                                               show one page of the diff between two snapshots, print JSON
  agent-snap ping                              print protocol version info
  agent-snap serve                             JSONL request/response server on stdin/stdout
  agent-snap version                           print version

Snapshots are stored as hidden git refs (refs/agent-snapshots/...). HEAD and the
real index are never touched.`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "agent-snap:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stdout, usage)
		return nil
	}
	switch args[0] {
	case "serve":
		if len(args) > 1 {
			return fmt.Errorf("command %q takes no arguments", args[0])
		}
		return protocol.Serve(context.Background(), os.Stdin, stdout)
	case "ping":
		if len(args) > 1 {
			return fmt.Errorf("command %q takes no arguments", args[0])
		}
		return printJSON(stdout, map[string]string{"status": "ok", "version": protocol.Version})
	case "version":
		if len(args) > 1 {
			return fmt.Errorf("command %q takes no arguments", args[0])
		}
		fmt.Fprintln(stdout, protocol.Version)
		return nil
	case "create":
		cwd, reason, err := flags(args[1:])
		if err != nil {
			return err
		}
		if !snapshot.ValidReason(reason) {
			return fmt.Errorf("%w: it contains a NUL byte or ends with an origin paragraph", snapshot.ErrInvalidReason)
		}
		store, err := snapshot.New(cwd)
		if err != nil {
			return err
		}
		snap, err := store.Create(context.Background(), reason, snapshot.Origin{Source: "cli"})
		if err != nil {
			return err
		}
		return printJSON(stdout, snap)
	case "list":
		cwd, limit, err := listFlags(args[1:])
		if err != nil {
			return err
		}
		store, err := snapshot.New(cwd)
		if err != nil {
			return err
		}
		items, err := store.List(context.Background(), limit)
		if err != nil {
			return err
		}
		return printJSON(stdout, map[string]any{"snapshots": items})
	case "get":
		cwd, id, err := getFlags(args[1:])
		if err != nil {
			return err
		}
		store, err := snapshot.New(cwd)
		if err != nil {
			return err
		}
		detail, err := store.Get(context.Background(), id)
		if err != nil {
			return err
		}
		return printJSON(stdout, detail)
	case "diff":
		cwd, from, to, page, err := diffFlags(args[1:])
		if err != nil {
			return err
		}
		store, err := snapshot.New(cwd)
		if err != nil {
			return err
		}
		diff, err := store.Diff(context.Background(), from, to, page)
		if err != nil {
			return err
		}
		return printJSON(stdout, diff)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func flags(args []string) (cwd, reason string, err error) {
	cwd, err = os.Getwd()
	if err != nil {
		cwd = "."
	}
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cwd", "--reason":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("flag %s requires a value", args[i])
			}
			if args[i] == "--cwd" {
				cwd = args[i+1]
			} else {
				reason = args[i+1]
			}
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", fmt.Errorf("unknown flag %q", args[i])
			}
			positional = append(positional, args[i])
		}
	}
	if len(positional) > 1 {
		return "", "", fmt.Errorf("expected a single positional reason, got %d", len(positional))
	}
	if reason != "" && len(positional) == 1 {
		return "", "", fmt.Errorf("conflicting reason arguments: both --reason and a positional reason were given")
	}
	if reason == "" && len(positional) == 1 {
		reason = positional[0]
	}
	return cwd, reason, nil
}

func listFlags(args []string) (cwd string, limit int, err error) {
	cwd, err = os.Getwd()
	if err != nil {
		cwd = "."
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cwd", "--limit":
			if i+1 >= len(args) {
				return "", 0, fmt.Errorf("flag %s requires a value", args[i])
			}
			if args[i] == "--cwd" {
				cwd = args[i+1]
			} else {
				limit, err = strconv.Atoi(args[i+1])
				if err != nil || limit <= 0 {
					return "", 0, fmt.Errorf("invalid value for --limit: %q", args[i+1])
				}
			}
			i++
		default:
			return "", 0, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return cwd, limit, nil
}

func getFlags(args []string) (cwd, id string, err error) {
	cwd, err = os.Getwd()
	if err != nil {
		cwd = "."
	}
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cwd":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("flag %s requires a value", args[i])
			}
			cwd = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", fmt.Errorf("unknown flag %q", args[i])
			}
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 1 {
		return "", "", fmt.Errorf("expected exactly one snapshot id, got %d", len(positional))
	}
	if !snapshot.ValidID(positional[0]) {
		return "", "", fmt.Errorf("invalid snapshot id %q", positional[0])
	}
	return cwd, positional[0], nil
}

func diffFlags(args []string) (cwd, from, to string, page snapshot.DiffPage, err error) {
	cwd, err = os.Getwd()
	if err != nil {
		cwd = "."
	}
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cwd", "--offset", "--max-patch-bytes":
			if i+1 >= len(args) {
				return "", "", "", page, fmt.Errorf("flag %s requires a value", args[i])
			}
			value := args[i+1]
			switch args[i] {
			case "--cwd":
				cwd = value
			case "--offset":
				if page.Offset, err = intFlag(args[i], value, 0, snapshot.MaxPageOffset); err != nil {
					return "", "", "", page, err
				}
			default:
				if page.MaxPatchBytes, err = intFlag(args[i], value, snapshot.DiffMinPatchBytes, snapshot.DiffMaxPatchBytes); err != nil {
					return "", "", "", page, err
				}
			}
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", "", page, fmt.Errorf("unknown flag %q", args[i])
			}
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 2 {
		return "", "", "", page, fmt.Errorf("expected two snapshot ids, got %d", len(positional))
	}
	for _, id := range positional {
		if !snapshot.ValidID(id) {
			return "", "", "", page, fmt.Errorf("invalid snapshot id %q", id)
		}
	}
	return cwd, positional[0], positional[1], page, nil
}

func intFlag(name, value string, min, max int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("invalid value for %s: %q (want an integer from %d through %d)", name, value, min, max)
	}
	return n, nil
}

func printJSON(stdout io.Writer, v any) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
