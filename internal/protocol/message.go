package protocol

import (
	"encoding/json"
	"time"
)

const (
	TypeAuth             = "auth"
	TypeAuthOK           = "auth_ok"
	TypeError            = "error"
	TypeMessage          = "message"
	TypeBroadcast        = "broadcast"
	TypeAgentJoined      = "agent_joined"
	TypeAgentLeft        = "agent_left"
	TypeAgentReconnected = "agent_reconnected"
	TypeListAgents       = "list_agents"
	TypeAgentsList       = "agents_list"
	TypeTaskAssign       = "task_assign"
	TypeTaskStatus       = "task_status"
	TypeTaskResult       = "task_result"

	TypeScratchpadGet    = "scratchpad_get"
	TypeScratchpadSet    = "scratchpad_set"
	TypeScratchpadDelete = "scratchpad_delete"
	TypeScratchpadList   = "scratchpad_list"
	TypeScratchpadResult = "scratchpad_result"
	TypeScratchpadUpdate = "scratchpad_update"

	TypeLeaderQuery    = "leader_query"
	TypeLeaderInfo     = "leader_info"
	TypeLeaderTransfer = "leader_transfer"

	TypeHistoryRequest = "history_request"
	TypeHistoryResult  = "history_result"

	TypeFileShare = "file_share"
)

type Envelope struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	From      string          `json:"from,omitempty"`
	To        string          `json:"to,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Sequence  int64           `json:"sequence,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
}

func NewEnvelope(msgType, sessionID, from, to string, payload any) (Envelope, error) {
	var raw json.RawMessage
	if payload != nil {
		var err error
		raw, err = json.Marshal(payload)
		if err != nil {
			return Envelope{}, err
		}
	}
	return Envelope{
		Type:      msgType,
		SessionID: sessionID,
		From:      from,
		To:        to,
		Payload:   raw,
		Timestamp: time.Now().UTC(),
	}, nil
}

func NewError(sessionID, errMsg string) Envelope {
	payload, _ := json.Marshal(map[string]string{"error": errMsg})
	return Envelope{
		Type:      TypeError,
		SessionID: sessionID,
		Payload:   payload,
		Timestamp: time.Now().UTC(),
	}
}

type AuthPayload struct {
	SessionID    string   `json:"session_id"`
	AgentID      string   `json:"agent_id"`
	AgentName    string   `json:"agent_name"`
	PSK          string   `json:"psk"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type AgentInfo struct {
	AgentID      string   `json:"agent_id"`
	AgentName    string   `json:"agent_name"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type ScratchpadSetPayload struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type ScratchpadListResult struct {
	Entries []ScratchpadEntry `json:"entries"`
}

type ScratchpadEntry struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedBy string          `json:"updated_by"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type LeaderTransferPayload struct {
	NewLeaderID string `json:"new_leader_id"`
}

type HistoryRequestPayload struct {
	AfterSequence int64 `json:"after_sequence,omitempty"`
	Limit         int   `json:"limit,omitempty"`
}

type TaskAssignPayload struct {
	TaskID      string          `json:"task_id"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type TaskStatusPayload struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type TaskResultPayload struct {
	TaskID string          `json:"task_id"`
	Result json.RawMessage `json:"result"`
}

type FileSharePayload struct {
	FileID      string `json:"file_id"`
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Description string `json:"description,omitempty"`
}
