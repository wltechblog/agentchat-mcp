package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wltechblog/agentchat-mcp/internal/protocol"
)

const version = "1.0.0"

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorWhite  = "\033[37m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
)

var (
	colors = []string{
		"\033[36m", // cyan
		"\033[33m", // yellow
		"\033[35m", // purple
		"\033[32m", // green
		"\033[34m", // blue
		"\033[91m", // bright red
		"\033[96m", // bright cyan
		"\033[93m", // bright yellow
	}
	agentColors = map[string]string{}
	colorIdx    = 0
)

func getAgentColor(agentID string) string {
	if c, ok := agentColors[agentID]; ok {
		return c
	}
	c := colors[colorIdx%len(colors)]
	agentColors[agentID] = c
	colorIdx++
	return c
}

type CLI struct {
	serverURL string
	sessionID string
	psk       string
	agentID   string
	agentName string
	client    *http.Client
	// lastSeq is the highest envelope sequence shown; used to dedupe the
	// same envelope arriving via history replay, live SSE, and mailbox
	// drain. Only touched from the watch goroutine.
	lastSeq int64
}

func main() {
	serverURL := flag.String("url", "", "Agentchat server URL (e.g. http://localhost:8080)")
	sessionID := flag.String("session", "", "Session ID to connect to")
	psk := flag.String("psk", "", "Session PSK")
	agentID := flag.String("agent", "human", "Your agent ID (default: human)")
	agentName := flag.String("name", "", "Your display name (default: agent ID)")
	listSessions := flag.Bool("list", false, "List available sessions and exit")
	showVersion := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("agentchat-cli %s\n", version)
		os.Exit(0)
	}

	// Read from env vars as fallback
	if *serverURL == "" {
		*serverURL = os.Getenv("AGENTCHAT_URL")
	}
	if *sessionID == "" {
		*sessionID = os.Getenv("AGENTCHAT_SESSION_ID")
	}
	if *psk == "" {
		*psk = os.Getenv("AGENTCHAT_PSK")
	}

	if *serverURL == "" {
		fmt.Fprintf(os.Stderr, "Error: -url or AGENTCHAT_URL required\n")
		flag.Usage()
		os.Exit(1)
	}

	*serverURL = strings.TrimSuffix(*serverURL, "/")

	cli := &CLI{
		serverURL: *serverURL,
		sessionID: *sessionID,
		psk:       *psk,
		agentID:   *agentID,
		agentName: *agentName,
		client:    &http.Client{Timeout: 30 * time.Second},
	}

	if *listSessions {
		cli.listSessions()
		return
	}

	if *sessionID == "" || *psk == "" {
		fmt.Fprintf(os.Stderr, "Error: -session and -psk required (or set AGENTCHAT_SESSION_ID and AGENTCHAT_PSK)\n")
		flag.Usage()
		os.Exit(1)
	}

	if cli.agentName == "" {
		cli.agentName = cli.agentID
	}

	cli.run()
}

func (c *CLI) doRequest(method, path string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.serverURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.psk)
	req.Header.Set("X-Agent-ID", c.agentID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.client.Do(req)
}

func (c *CLI) doJSON(method, path string, payload any) (any, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	resp, err := c.doRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rbody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server error (%d): %s", resp.StatusCode, string(rbody))
	}
	var result any
	json.Unmarshal(rbody, &result)
	return result, nil
}

func (c *CLI) listSessions() {
	resp, err := c.doRequest("GET", "/watch/sessions?session=list&psk=list", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
}

func (c *CLI) run() {
	// Register as an agent
	_, err := c.doJSON("POST", "/sessions/"+c.sessionID+"/register", map[string]any{
		"capabilities": []string{"chat", "human"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%s%s╔══════════════════════════════════════════════════╗%s\n", colorBold, colorCyan, colorReset)
	fmt.Printf("%s%s║  AgentChat CLI — connected to session %s%s%s%s ║%s\n",
		colorBold, colorCyan, colorGreen, c.sessionID[:8], colorCyan, strings.Repeat(" ", max(0, 32-len(c.sessionID[:8]))), colorReset)
	fmt.Printf("%s%s╚══════════════════════════════════════════════════╝%s\n", colorBold, colorCyan, colorReset)
	fmt.Printf("%sType messages to broadcast. Use /help for commands.%s\n\n", colorDim, colorReset)

	quit := make(chan struct{})
	defer close(quit)

	go c.watchLoop(quit)
	go c.heartbeatLoop(quit)

	// Handle input
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	scanner := bufio.NewScanner(os.Stdin)
	for {
		select {
		case <-sigCh:
			fmt.Printf("\n%sGoodbye!%s\n", colorYellow, colorReset)
			return
		default:
			if !scanner.Scan() {
				return
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "/") {
				c.handleCommand(line)
			} else {
				c.sendBroadcast(line)
			}
		}
	}
}

// heartbeatLoop keeps the CLI's presence alive; the server expires agents
// after 60s of inactivity, and an idle human at the terminal is exactly the
// kind of agent that would otherwise vanish.
func (c *CLI) heartbeatLoop(quit <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-quit:
			return
		case <-ticker.C:
			if _, err := c.doJSON("POST", "/sessions/"+c.sessionID+"/register", map[string]any{
				"capabilities": []string{"chat", "human"},
			}); err != nil {
				fmt.Fprintf(os.Stderr, "%sheartbeat failed: %v%s\n", colorDim, err, colorReset)
			}
		}
	}
}

// watchLoop keeps an SSE connection alive with capped exponential backoff.
func (c *CLI) watchLoop(quit <-chan struct{}) {
	backoff := time.Second
	const maxBackoff = 10 * time.Second
	const healthyUptime = 30 * time.Second

	for {
		select {
		case <-quit:
			return
		default:
		}

		started := time.Now()
		err := c.watchSSE(quit)

		select {
		case <-quit:
			return
		default:
		}

		if err != nil {
			fmt.Printf("%sSSE connection lost (%v). Reconnecting in %v...%s\n", colorRed, err, backoff, colorReset)
		}

		if time.Since(started) >= healthyUptime {
			backoff = time.Second
		} else if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}

		select {
		case <-quit:
			return
		case <-time.After(backoff):
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (c *CLI) handleCommand(cmd string) {
	parts := strings.SplitN(cmd, " ", 3)
	switch parts[0] {
	case "/help":
		fmt.Printf("%sCommands:%s\n", colorBold, colorReset)
		fmt.Println("  /help              — Show this help")
		fmt.Println("  /msg <agent> <text> — Send direct message")
		fmt.Println("  /agents            — List online agents")
		fmt.Println("  /history           — Show message history")
		fmt.Println("  /scratchpad        — Show shared scratchpad")
		fmt.Println("  /quit              — Exit")

	case "/quit", "/exit":
		fmt.Printf("%sGoodbye!%s\n", colorYellow, colorReset)
		os.Exit(0)

	case "/msg":
		if len(parts) < 3 {
			fmt.Printf("%sUsage: /msg <agent_id> <message>%s\n", colorYellow, colorReset)
			return
		}
		c.sendDirect(parts[1], parts[2])

	case "/agents":
		c.showAgents()

	case "/history":
		c.showHistory()

	case "/scratchpad":
		c.showScratchpad()

	default:
		fmt.Printf("%sUnknown command: %s. Type /help for commands.%s\n", colorRed, cmd, colorReset)
	}
}

func (c *CLI) sendBroadcast(text string) {
	_, err := c.doJSON("POST", "/sessions/"+c.sessionID+"/broadcast", map[string]any{
		"type": "message",
		"payload": map[string]any{
			"text": text,
		},
	})
	if err != nil {
		fmt.Printf("%sSend failed: %v%s\n", colorRed, err, colorReset)
	}
}

func (c *CLI) sendDirect(to, text string) {
	_, err := c.doJSON("POST", "/sessions/"+c.sessionID+"/messages", map[string]any{
		"to":   to,
		"type": "message",
		"payload": map[string]any{
			"text": text,
		},
	})
	if err != nil {
		fmt.Printf("%sSend failed: %v%s\n", colorRed, err, colorReset)
	} else {
		fmt.Printf("%s  → %s%s\n", colorDim, to, colorReset)
	}
}

func (c *CLI) showAgents() {
	result, err := c.doJSON("GET", "/sessions/"+c.sessionID+"/agents", nil)
	if err != nil {
		fmt.Printf("%sError: %v%s\n", colorRed, err, colorReset)
		return
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Printf("%sAgents:%s\n%s\n", colorBold, colorReset, string(data))
}

func (c *CLI) showHistory() {
	result, err := c.doJSON("GET", "/sessions/"+c.sessionID+"/history", nil)
	if err != nil {
		fmt.Printf("%sError: %v%s\n", colorRed, err, colorReset)
		return
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Printf("%sHistory:%s\n%s\n", colorBold, colorReset, string(data))
}

func (c *CLI) showScratchpad() {
	result, err := c.doJSON("GET", "/sessions/"+c.sessionID+"/scratchpad", nil)
	if err != nil {
		fmt.Printf("%sError: %v%s\n", colorRed, err, colorReset)
		return
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Printf("%sScratchpad:%s\n%s\n", colorBold, colorReset, string(data))
}

// sseFrame is a complete Server-Sent Events frame.
type sseFrame struct {
	Event string
	Data  string
}

// sseParser assembles SSE frames from raw lines, handling multi-line data,
// comments, CRLF line endings, and retry/id fields per the SSE spec.
type sseParser struct {
	event   string
	data    []string
	hasData bool
}

// feed consumes one raw line and returns a frame when a blank line completes
// one.
func (p *sseParser) feed(line string) (sseFrame, bool) {
	line = strings.TrimSuffix(line, "\r")

	switch {
	case line == "":
		if !p.hasData {
			return sseFrame{}, false
		}
		f := sseFrame{Event: p.event, Data: strings.Join(p.data, "\n")}
		p.event, p.data, p.hasData = "", nil, false
		return f, true
	case strings.HasPrefix(line, ":"):
		return sseFrame{}, false // comment / keepalive
	case strings.HasPrefix(line, "event:"):
		p.event = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " ")
		return sseFrame{}, false
	case strings.HasPrefix(line, "data:"):
		p.hasData = true
		p.data = append(p.data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		return sseFrame{}, false
	case strings.HasPrefix(line, "retry:"), strings.HasPrefix(line, "id:"):
		// The CLI uses its own backoff schedule and has no resume support.
		return sseFrame{}, false
	}
	return sseFrame{}, false
}

func (c *CLI) watchSSE(quit <-chan struct{}) error {
	url := fmt.Sprintf("%s/watch?session=%s&psk=%s", c.serverURL, c.sessionID, c.psk)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{Timeout: 0} // no timeout: the stream is long-lived
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var p sseParser
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-quit:
			return nil
		default:
		}

		frame, ok := p.feed(scanner.Text())
		if !ok {
			continue
		}

		switch frame.Event {
		case "connected":
			// (Re)connected: drain the mailbox so messages missed while the
			// stream was down are shown (deduped against live/history).
			c.drainAndDisplay()
		case "history", "message":
			var env protocol.Envelope
			if err := json.Unmarshal([]byte(frame.Data), &env); err == nil {
				c.showEnvelope(frame.Event, env)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("stream ended")
}

// showEnvelope displays an envelope, deduping by sequence number so the same
// event arriving via history replay, live SSE, and mailbox drain shows once.
func (c *CLI) showEnvelope(eventType string, env protocol.Envelope) {
	if env.Sequence > 0 {
		if env.Sequence <= c.lastSeq {
			return
		}
		c.lastSeq = env.Sequence
	}
	c.displayEnvelope(eventType, env)
}

// drainAndDisplay drains this agent's server-side mailbox and displays
// everything in it — this is how the human sees direct messages at all,
// and whatever queued while the SSE stream was down.
func (c *CLI) drainAndDisplay() {
	result, err := c.doJSON("GET", "/sessions/"+c.sessionID+"/mailbox", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%smailbox drain failed: %v%s\n", colorDim, err, colorReset)
		return
	}
	data, _ := json.Marshal(result)
	var resp struct {
		Messages []struct {
			Envelope protocol.Envelope `json:"envelope"`
		} `json:"messages"`
	}
	if json.Unmarshal(data, &resp) != nil {
		return
	}
	for _, m := range resp.Messages {
		c.showEnvelope("mailbox", m.Envelope)
	}
}

func (c *CLI) displayEnvelope(eventType string, env protocol.Envelope) {
	timestamp := env.Timestamp.Format("15:04:05")

	switch env.Type {
	case protocol.TypeAgentJoined:
		info := parseAgentInfo(env.Payload)
		fmt.Printf("\n%s%s → %s joined the session%s\n",
			colorGreen, timestamp, info.AgentID, colorReset)

	case protocol.TypeAgentLeft:
		info := parseAgentInfo(env.Payload)
		fmt.Printf("\n%s%s ← %s left the session%s\n",
			colorRed, timestamp, info.AgentID, colorReset)

	case protocol.TypeMessage:
		from := env.From
		color := getAgentColor(from)
		payload := parsePayloadText(env.Payload)
		if env.To == "" || env.To == "*" {
			// Broadcast
			fmt.Printf("\n%s%s %s%s%s [broadcast]%s: %s\n",
				colorDim, timestamp, color, from, colorDim, colorReset, payload)
		} else {
			// Direct message
			fmt.Printf("\n%s%s %s%s%s → %s%s%s: %s\n",
				colorDim, timestamp, color, from, colorDim,
				colorCyan, env.To, colorReset, payload)
		}

	case protocol.TypeBroadcast:
		from := env.From
		color := getAgentColor(from)
		payload := parsePayloadText(env.Payload)
		fmt.Printf("\n%s%s %s%s%s [broadcast]%s: %s\n",
			colorDim, timestamp, color, from, colorDim, colorReset, payload)

	case protocol.TypeTaskAssign:
		from := env.From
		color := getAgentColor(from)
		fmt.Printf("\n%s%s %s%s%s → %s%s [task_assign]%s: %s\n",
			colorDim, timestamp, color, from, colorDim,
			colorCyan, env.To, colorReset, string(env.Payload))

	case protocol.TypeTaskResult:
		from := env.From
		color := getAgentColor(from)
		fmt.Printf("\n%s%s %s%s%s → %s%s [task_result]%s: %s\n",
			colorDim, timestamp, color, from, colorDim,
			colorCyan, env.To, colorReset, string(env.Payload))

	case protocol.TypeLeaderInfo:
		fmt.Printf("\n%s%s ⭐ Leader: %s%s\n",
			colorYellow, timestamp, string(env.Payload), colorReset)

	default:
		from := env.From
		color := getAgentColor(from)
		fmt.Printf("\n%s%s %s%s%s [%s]: %s\n",
			colorDim, timestamp, color, from, colorReset, env.Type, string(env.Payload))
	}

	// Print input prompt
	fmt.Print("> ")
}

func parsePayloadText(payload json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err == nil {
		if text, ok := m["text"].(string); ok {
			return text
		}
		if content, ok := m["content"].(string); ok {
			return content
		}
		// Pretty print the whole payload
		data, _ := json.Marshal(m)
		return string(data)
	}
	return string(payload)
}

func parseAgentInfo(payload json.RawMessage) protocol.AgentInfo {
	var info protocol.AgentInfo
	json.Unmarshal(payload, &info)
	return info
}
