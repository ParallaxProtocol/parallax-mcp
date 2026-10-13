// Command mcpcall is a small MCP client for trying the server from a shell.
// It launches parallax-mcp over stdio (or connects to a running HTTP
// server), makes one request and prints the result.
//
//	mcpcall [-http URL] [-server PATH] tools
//	mcpcall [-http URL] [-server PATH] call get_block '{"block":"latest"}'
//	mcpcall resources | read parallax://glossary
//	mcpcall prompts | prompt audit_address '{"address":"0x…"}'
//
// Flags after "--" are passed to the launched server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	httpURL := flag.String("http", "", "connect to this Streamable HTTP endpoint instead of launching a server")
	bin := flag.String("server", "./bin/parallax-mcp", "server binary to launch over stdio")
	timeout := flag.Duration("timeout", 90*time.Second, "overall timeout")
	flag.Parse()

	args := flag.Args()
	var serverArgs []string
	for i, a := range args {
		if a == "--" {
			args, serverArgs = args[:i], args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mcpcall [flags] tools|call NAME [JSON]|resources|read URI|prompts|prompt NAME [JSON] [-- server flags]")
		os.Exit(2)
	}
	if err := run(*httpURL, *bin, serverArgs, args, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "mcpcall:", err)
		os.Exit(1)
	}
}

func run(httpURL, bin string, serverArgs, args []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var t mcp.Transport
	if httpURL != "" {
		t = &mcp.StreamableClientTransport{Endpoint: httpURL}
	} else {
		cmd := exec.Command(bin, serverArgs...)
		cmd.Stderr = os.Stderr
		if os.Getenv("MCPCALL_QUIET") != "" {
			cmd.Stderr = nil
		}
		t = &mcp.CommandTransport{Command: cmd}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcpcall", Version: "dev"}, nil)
	cs, err := client.Connect(ctx, t, nil)
	if err != nil {
		return err
	}
	defer cs.Close()

	arg := func(i int) map[string]any {
		if len(args) <= i {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(args[i]), &m); err != nil {
			fmt.Fprintln(os.Stderr, "mcpcall: arguments must be a JSON object:", err)
			os.Exit(2)
		}
		return m
	}

	switch args[0] {
	case "tools":
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			return err
		}
		for _, tool := range res.Tools {
			desc, _, _ := strings.Cut(tool.Description, "\n")
			fmt.Printf("%-24s %s\n", tool.Name, desc)
		}
	case "schema":
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			return err
		}
		for _, tool := range res.Tools {
			if len(args) > 1 && tool.Name != args[1] {
				continue
			}
			return print(tool)
		}
	case "call":
		if len(args) < 2 {
			return fmt.Errorf("call needs a tool name")
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: args[1], Arguments: arg(2)})
		if err != nil {
			return err
		}
		if res.IsError {
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					fmt.Println("TOOL ERROR:", tc.Text)
				}
			}
			os.Exit(3)
		}
		if res.StructuredContent != nil {
			return print(res.StructuredContent)
		}
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				fmt.Println(tc.Text)
			}
		}
	case "resources":
		res, err := cs.ListResources(ctx, nil)
		if err != nil {
			return err
		}
		for _, r := range res.Resources {
			fmt.Printf("%-44s %s\n", r.URI, r.Name)
		}
		tmpl, err := cs.ListResourceTemplates(ctx, nil)
		if err == nil {
			for _, r := range tmpl.ResourceTemplates {
				fmt.Printf("%-44s %s (template)\n", r.URITemplate, r.Name)
			}
		}
	case "read":
		if len(args) < 2 {
			return fmt.Errorf("read needs a URI")
		}
		res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: args[1]})
		if err != nil {
			return err
		}
		for _, c := range res.Contents {
			fmt.Println(c.Text)
		}
	case "prompts":
		res, err := cs.ListPrompts(ctx, nil)
		if err != nil {
			return err
		}
		for _, p := range res.Prompts {
			fmt.Printf("%-24s %s\n", p.Name, p.Description)
		}
	case "prompt":
		if len(args) < 2 {
			return fmt.Errorf("prompt needs a name")
		}
		params := &mcp.GetPromptParams{Name: args[1], Arguments: map[string]string{}}
		for k, v := range arg(2) {
			params.Arguments[k] = fmt.Sprint(v)
		}
		res, err := cs.GetPrompt(ctx, params)
		if err != nil {
			return err
		}
		for _, m := range res.Messages {
			if tc, ok := m.Content.(*mcp.TextContent); ok {
				fmt.Printf("[%s]\n%s\n", m.Role, tc.Text)
			}
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}

func print(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
