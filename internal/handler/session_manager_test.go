package handler

import (
	"testing"

	"aurora/internal/chatgpt"
)

func TestSessionManagerRegistersResponsesState(t *testing.T) {
	manager := &SessionManager{
		sessions:         make(map[string]*sessionEntry),
		responseSessions: make(map[string]*responseSessionEntry),
		ttl:              defaultSessionTTL,
	}
	state := chatgpt.NewChatClientState()
	state.ConversationID = "conv-1"
	state.ParentMessageID = "msg-1"
	manager.RegisterResponse("resp-1", state, map[string]string{"call-1": "bash"})

	gotState, names := manager.GetResponse("resp-1")
	if gotState == state {
		t.Fatal("response state should be an immutable snapshot, not the original pointer")
	}
	if gotState.ConversationID != state.ConversationID || gotState.ParentMessageID != state.ParentMessageID {
		t.Fatalf("state = %#v, want %#v", gotState, state)
	}
	if names["call-1"] != "bash" {
		t.Fatalf("call name = %q, want bash", names["call-1"])
	}
}
