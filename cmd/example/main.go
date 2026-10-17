// Command example chats with a harness agent in the terminal.
//
//	OPENAI_API_KEY=sk-... go run ./cmd/example -model gpt-5-mini
//	go run ./cmd/example -base-url http://localhost:11434/v1 -model qwen3 -dialect ollama
//	go run ./cmd/example -fake
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/xkamail/harness"
	"github.com/xkamail/harness/harnesstest"
	"github.com/xkamail/harness/openaiharness"
)

var agent = harness.MustCompile(harness.Agent{
	Name:         "example",
	SystemPrompt: "You are a terse assistant. Use tools when they help.",
	Tools: []harness.Tool{
		{
			Name:        "now",
			Description: "Call when you need the current time.",
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				return time.Now().Format(time.RFC3339), nil
			},
		},
		{
			Name:        "delete_file",
			Description: "Call to delete a file by path.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			Tier:        harness.TierDestructive,
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var in struct{ Path string }
				if err := json.Unmarshal(args, &in); err != nil {
					return nil, err
				}
				return "pretended to delete " + in.Path, nil
			},
		},
	},
})

func main() {
	var (
		modelID = flag.String("model", "gpt-5-mini", "model id")
		baseURL = flag.String("base-url", "", "OpenAI-compatible endpoint; empty is OpenAI")
		dialect = flag.String("dialect", "openai", "openai|deepseek|ollama")
		fake    = flag.Bool("fake", false, "use a scripted model, no network")
	)
	flag.Parse()

	m := newModel(*modelID, *baseURL, *dialect, *fake)
	ctx := context.Background()
	in := bufio.NewScanner(os.Stdin)
	var history []harness.Message
	var grants harness.Grants

	fmt.Print("> ")
	for in.Scan() {
		history = append(history, harness.Message{Role: harness.RoleUser, Content: in.Text()})
		grants = harness.Grants{} // a new instruction ends any pre-approval
		var decision *harness.DecisionPayload
		for {
			res, err := agent.Run(ctx, m, harness.RunConfig{
				History:  history,
				Grants:   grants,
				Decision: decision,
				OnDelta:  func(_, text string) { fmt.Print(text) },
			})
			history = append(history, res.Messages...)
			if err != nil {
				log.Printf("run: %v", err)
				break
			}
			fmt.Printf("\n[%s · %d iters · in=%d out=%d]\n",
				res.Stop, res.Iterations, res.Usage.InputTokens, res.Usage.OutputTokens)

			if res.Stop != harness.StopConfirm {
				break
			}
			group := agent.GrantGroup(res.Confirm.Name)
			prompt := "[y/N]"
			if len(group) > 0 {
				prompt = "[y/a/N] (a: allow " + strings.Join(group, ", ") + " for this task)"
			}
			fmt.Printf("allow %s %s? %s ", res.Confirm.Name, res.Confirm.Arguments, prompt)
			if !in.Scan() {
				return
			}
			decision = &harness.DecisionPayload{ApprovalID: res.Confirm.ID, Decision: harness.DecisionReject}
			switch in.Text() {
			case "y":
				decision.Decision = harness.DecisionApprove
			case "a":
				if len(group) > 0 {
					decision.Decision = harness.DecisionApproveAll
					grants.Tools = append(grants.Tools, group...)
				}
			}
		}
		fmt.Print("> ")
	}
}

func newModel(id, baseURL, dialect string, fake bool) harness.Model {
	if fake {
		return harnesstest.NewFakeModel(
			harnesstest.Call("c1", "now", struct{}{}),
			harnesstest.Text("It is now."),
		)
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" && baseURL == "" {
		log.Fatal("set OPENAI_API_KEY, or pass -base-url or -fake")
	}
	opts := []openaiharness.Option{openaiharness.WithProvider(dialect)}
	if baseURL != "" {
		opts = append(opts, openaiharness.WithBaseURL(baseURL))
	}
	switch dialect {
	case "deepseek":
		opts = append(opts, openaiharness.WithDialect(openaiharness.DeepSeek))
	case "ollama":
		opts = append(opts, openaiharness.WithDialect(openaiharness.Ollama))
	}
	return openaiharness.New(key, id, opts...)
}
