package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// TestSubMCPSamplingIsForwardedToTheAgent pins the direction sampling is meant to
// travel: a sub-MCP asks the client's model to generate, and Termcp carries the
// request to the Agent driving the tool call.
//
// This is what sampling is FOR — the protocol's own description is "the server asks
// the client's LLM to generate". It is not a notification channel, which is why the
// `watch` directive that used it as an alarm is gone.
func TestSubMCPSamplingIsForwardedToTheAgent(t *testing.T) {
	s := newTestServer(t)

	// A sub-MCP on a pipe: it emits one sampling request, then echoes whatever
	// answer it gets, so the test can see the round trip.
	script := writeScript(t, `
printf '{"jsonrpc":"2.0","id":7,"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"summarise"}}],"maxTokens":16}}\n'
read answer
printf 'GOT_ANSWER:%s\n' "$answer"
`)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "connect internal --argv sh '" + script + "'"}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// The Agent session is taken from the context, so the call must carry one that
	// implements sampling.
	fake := &fakeSamplingSession{answer: "the model said yes"}
	ctx := s.mcpServer.WithContext(context.Background(), fake)

	got := termTextWithCtx(t, s, ctx, map[string]any{"cmd": "read " + h + " --frame"})

	// The forwarded request reached the Agent...
	if fake.calls == 0 {
		t.Fatalf("the sampling request was never forwarded to the Agent; reply was:\n%s", got)
	}
	if fake.lastPrompt != "summarise" {
		t.Errorf("the Agent saw prompt %q, want %q", fake.lastPrompt, "summarise")
	}

	// ...and recv did NOT hand the request to the caller as if it were the frame
	// they asked for.
	if strings.Contains(got, "sampling/createMessage") {
		t.Errorf("recv returned the sub-MCP's request instead of answering it:\n%s", got)
	}
}

// fakeSamplingSession is an MCP client session that answers sampling requests.
type fakeSamplingSession struct {
	mcpserver.ClientSession
	answer     string
	calls      int
	lastPrompt string
}

func (f *fakeSamplingSession) RequestSampling(_ context.Context, req mcpgo.CreateMessageRequest) (*mcpgo.CreateMessageResult, error) {
	f.calls++
	if len(req.Messages) > 0 {
		// Content is `any`: a request that arrived as JSON decodes to a map, while one
		// built in Go holds a TextContent. Both are legitimate callers.
		switch c := req.Messages[0].Content.(type) {
		case mcpgo.TextContent:
			f.lastPrompt = c.Text
		case map[string]any:
			if txt, ok := c["text"].(string); ok {
				f.lastPrompt = txt
			}
		case *mcpgo.TextContent:
			f.lastPrompt = c.Text
		}
	}
	res := &mcpgo.CreateMessageResult{
		Model:      "fake-model",
		StopReason: "endTurn",
	}
	res.Role = mcpgo.RoleAssistant
	res.Content = mcpgo.NewTextContent(f.answer)
	return res, nil
}

func (f *fakeSamplingSession) Initialize()       {}
func (f *fakeSamplingSession) Initialized() bool { return true }
func (f *fakeSamplingSession) SessionID() string { return "fake-session" }

// TestSamplingAnswerIsWrittenBackToTheSubMCP pins the second half: the Agent's
// answer reaches the sub-MCP, so its request is not left outstanding forever.
func TestSamplingAnswerIsWrittenBackToTheSubMCP(t *testing.T) {
	s := newTestServer(t)

	script := writeScript(t, `
printf '{"jsonrpc":"2.0","id":7,"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"go"}}],"maxTokens":8}}\n'
read answer
printf 'ECHO:%s\n' "$answer"
`)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "connect internal --argv sh '" + script + "'"}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	fake := &fakeSamplingSession{answer: "MODEL_OUTPUT"}
	ctx := s.mcpServer.WithContext(context.Background(), fake)

	// The recv answers the sampling request, writes the answer back, and then keeps
	// reading for a real frame — which never comes, so it reports 204.
	termTextWithCtx(t, s, ctx, map[string]any{"cmd": "read " + h + " --frame", "timeout": 3})

	// The sub-MCP reads the answer, echoes it, and exits. Reading the channel by
	// position shows the whole transcript regardless of when the echo landed.
	var out string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out = termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1"})
		if strings.Contains(out, "ECHO:") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(out, "MODEL_OUTPUT") {
		t.Fatalf("the Agent's answer never reached the sub-MCP:\n%s", out)
	}
}
