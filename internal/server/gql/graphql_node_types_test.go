package gql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
)

// TestGUIDTypeToNodeType_CoversEveryPersistedNode guards a gap that is silent
// and confusing: the Relay node lookup is driven by this hand-written map, so a
// node type that is generated, queryable and has an IsNode method still cannot
// have its id field resolved until it is listed here.
//
// The symptom is an "unknown node type" error on exactly the id field while
// every other field of the same object resolves fine, which makes it look like
// a field-level problem rather than a registration one.
func TestGUIDTypeToNodeType_CoversEveryPersistedNode(t *testing.T) {
	t.Parallel()

	// Every node type the schema persists and exposes. A new entity that is
	// meant to be reachable by id belongs here.
	expected := map[string]string{
		ent.TypeUser:                    "users",
		ent.TypeAPIKey:                  "api_keys",
		ent.TypeAPIKeyProfileTemplate:   "api_key_profile_templates",
		ent.TypeModel:                   "models",
		ent.TypeChannel:                 "channels",
		ent.TypeChannelAccount:          "channel_accounts",
		ent.TypeChannelProbe:            "channel_probes",
		ent.TypeChannelOverrideTemplate: "channel_override_templates",
		ent.TypeRequest:                 "requests",
		ent.TypeRequestExecution:        "request_executions",
		ent.TypeRole:                    "roles",
		ent.TypeSystem:                  "systems",
		ent.TypeUsageLog:                "usage_logs",
		ent.TypeProject:                 "projects",
		ent.TypeUserProject:             "user_projects",
		ent.TypeUserRole:                "user_roles",
		ent.TypeThread:                  "threads",
		ent.TypeTrace:                   "traces",
		ent.TypeDataStorage:             "data_storages",
		ent.TypePrompt:                  "prompts",
	}

	for guidType, table := range expected {
		t.Run(guidType, func(t *testing.T) {
			t.Parallel()

			got, ok := guidTypeToNodeType[guidType]
			require.True(t, ok, "guid type %q must be resolvable to a node table", guidType)
			require.Equal(t, table, got)
		})
	}
}

// TestGUIDTypeToNodeType_MapsChannelAccount is the specific regression: accounts
// are reachable as a channel edge, but without this entry their id could not be
// resolved, which breaks any client that selects it.
func TestGUIDTypeToNodeType_MapsChannelAccount(t *testing.T) {
	t.Parallel()

	got, ok := guidTypeToNodeType[ent.TypeChannelAccount]
	require.True(t, ok, "ChannelAccount must be registered so its id resolves")
	require.Equal(t, "channel_accounts", got)
}
