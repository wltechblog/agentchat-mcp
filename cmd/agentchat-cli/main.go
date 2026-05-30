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
		"agent_name":   c.agentName,
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

	// Start SSE watcher in background
	sseDone := make(chan struct{})
	go c.watchSSE(sseDone)

	// Handle input
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	scanner := bufio.NewScanner(os.Stdin)
	for {
		select {
		case <-sigCh:
			fmt.Printf("\n%sGoodbye!%s\n", colorYellow, colorReset)
			return
		case <-sseDone:
			fmt.Printf("\n%sSSE connection lost. Reconnecting...%s\n", colorRed, colorReset)
			// Create a fresh channel for the new goroutine
			sseDone = make(chan struct{})
			go c.watchSSE(sseDone)
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

func (c *CLI) watchSSE(done chan struct{}) {
	url := fmt.Sprintf("%s/watch?session=%s&psk=%s", c.serverURL, c.sessionID, c.psk)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Printf("%sSSE error: %v%s\n", colorRed, err, colorReset)
		close(done)
		return
	}
	req.Header.Set("Cache-Control", "no-cache")

	// Longer timeout for SSE
	client := &http.Client{Timeout: 0} // no timeout for SSE
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("%sSSE connection failed: %v%s\n", colorRed, err, colorReset)
		close(done)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("%sSSE error (%d): %s%s\n", colorRed, resp.StatusCode, string(body), colorReset)
		close(done)
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType := strings.TrimPrefix(line, "event: ")

			// Read data line
			if !scanner.Scan() {
				break
			}
			dataLine := scanner.Text()
			if !strings.HasPrefix(dataLine, "data: ") {
				continue
			}
			data := strings.TrimPrefix(dataLine, "data: ")

			// Skip connected event
			if eventType == "connected" {
				continue
			}

			// Parse envelope
			var env protocol.Envelope
			if err := json.Unmarshal([]byte(data), &env); err != nil {
				continue
			}

			// Display the message
			c.displayEnvelope(eventType, env)

			// Read empty line separator
			scanner.Scan()
		}
	}

	close(done)
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
