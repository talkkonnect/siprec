package sip

import (
	"testing"

	"siprec-server/pkg/siprec"
)

func TestDeriveCallerCallee(t *testing.T) {
	tests := []struct {
		name       string
		parts      []siprec.Participant
		wantCaller string
		wantCallee string
	}{
		{
			name: "explicit caller/callee roles",
			parts: []siprec.Participant{
				{Role: "callee", CommunicationIDs: []siprec.CommunicationID{{Value: "sip:2002@pbx"}}},
				{Role: "caller", CommunicationIDs: []siprec.CommunicationID{{Value: "sip:1001@pbx"}}},
			},
			wantCaller: "sip:1001@pbx",
			wantCallee: "sip:2002@pbx",
		},
		{
			name: "no roles falls back to order",
			parts: []siprec.Participant{
				{CommunicationIDs: []siprec.CommunicationID{{Value: "sip:1001@pbx"}}},
				{CommunicationIDs: []siprec.CommunicationID{{Value: "sip:2002@pbx"}}},
			},
			wantCaller: "sip:1001@pbx",
			wantCallee: "sip:2002@pbx",
		},
		{
			name: "calling/called party flags",
			parts: []siprec.Participant{
				{CalledParty: true, CommunicationIDs: []siprec.CommunicationID{{Value: "tel:+15550002"}}},
				{CallingParty: true, CommunicationIDs: []siprec.CommunicationID{{Value: "tel:+15550001"}}},
			},
			wantCaller: "tel:+15550001",
			wantCallee: "tel:+15550002",
		},
		{
			name: "aor priority wins over order",
			parts: []siprec.Participant{
				{Role: "caller", CommunicationIDs: []siprec.CommunicationID{
					{Value: "sip:low@pbx", Priority: 5},
					{Value: "sip:high@pbx", Priority: 1},
				}},
			},
			wantCaller: "sip:high@pbx",
			wantCallee: "",
		},
		{
			name: "name fallback when no comm ids",
			parts: []siprec.Participant{
				{Role: "caller", Name: "Alice"},
				{Role: "callee", DisplayName: "Bob"},
			},
			wantCaller: "Alice",
			wantCallee: "Bob",
		},
		{
			name:       "empty participants",
			parts:      nil,
			wantCaller: "",
			wantCallee: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCaller, gotCallee := deriveCallerCallee(tt.parts)
			if gotCaller != tt.wantCaller {
				t.Errorf("caller = %q, want %q", gotCaller, tt.wantCaller)
			}
			if gotCallee != tt.wantCallee {
				t.Errorf("callee = %q, want %q", gotCallee, tt.wantCallee)
			}
		})
	}
}

func TestDeriveCallerCalleeNames(t *testing.T) {
	tests := []struct {
		name           string
		parts          []siprec.Participant
		wantCallerName string
		wantCalleeName string
	}{
		{
			name: "display names line up with roles",
			parts: []siprec.Participant{
				{Role: "callee", DisplayName: "Bob", CommunicationIDs: []siprec.CommunicationID{{Value: "sip:2002@pbx"}}},
				{Role: "caller", DisplayName: "Alice", CommunicationIDs: []siprec.CommunicationID{{Value: "sip:1001@pbx"}}},
			},
			wantCallerName: "Alice",
			wantCalleeName: "Bob",
		},
		{
			name: "falls back to Name when no DisplayName",
			parts: []siprec.Participant{
				{Role: "caller", Name: "Alice Smith", CommunicationIDs: []siprec.CommunicationID{{Value: "sip:1001@pbx"}}},
				{Role: "callee", CommunicationIDs: []siprec.CommunicationID{{Value: "sip:2002@pbx"}}},
			},
			wantCallerName: "Alice Smith",
			wantCalleeName: "",
		},
		{
			name: "names follow participant order when roles absent",
			parts: []siprec.Participant{
				{DisplayName: "First", CommunicationIDs: []siprec.CommunicationID{{Value: "sip:1001@pbx"}}},
				{DisplayName: "Second", CommunicationIDs: []siprec.CommunicationID{{Value: "sip:2002@pbx"}}},
			},
			wantCallerName: "First",
			wantCalleeName: "Second",
		},
		{
			name:           "empty participants",
			parts:          nil,
			wantCallerName: "",
			wantCalleeName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCallerName, gotCalleeName := deriveCallerCalleeNames(tt.parts)
			if gotCallerName != tt.wantCallerName {
				t.Errorf("callerName = %q, want %q", gotCallerName, tt.wantCallerName)
			}
			if gotCalleeName != tt.wantCalleeName {
				t.Errorf("calleeName = %q, want %q", gotCalleeName, tt.wantCalleeName)
			}
		})
	}
}
