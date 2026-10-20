// Package mcp wires the CUDly MCP server: it registers every tool from
// tools/ onto a github.com/modelcontextprotocol/go-sdk/mcp.Server and
// builds the cudly_list_commitment_actions catalog from those same tools'
// descriptors, so the live tool set and the discoverability catalog can
// never drift apart.
package mcp

import (
	"bytes"
	"context"
	"fmt"

	gosdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeanerCloud/cloud-commitments-mcp/tools"
)

// ServerName is the MCP Implementation.Name this server identifies as.
const ServerName = "cudly-mcp"

// registrations returns every purchase/search tool this server exposes,
// excluding cudly_list_commitment_actions itself (NewServer adds that one
// last, once it has every other tool's Descriptor to build the catalog
// from).
func registrations() []tools.Registration {
	return []tools.Registration{
		tools.NewSearchRecommendationsTool(),
		tools.NewAWSEC2RIPurchaseTool(),
		tools.NewAWSOpenSearchRIPurchaseTool(),
		tools.NewAWSRedshiftRIPurchaseTool(),
		tools.NewAWSMemoryDBRIPurchaseTool(),
		tools.NewAWSRDSRIPurchaseTool(),
		tools.NewAWSElastiCacheRIPurchaseTool(),
		tools.NewAWSSavingsPlansPurchaseTool(),
		tools.NewAzureComputeRIPurchaseTool(),
		tools.NewGCPComputeEngineCUDPurchaseTool(),
		tools.NewArcheraComparisonTool(),
		tools.NewArcheraLineItemsTool(),
	}
}

// NewServer builds the CUDly MCP server with every tool registered. version
// is reported to clients as the server's Implementation.Version (pass the
// build-time version string, or "dev" for unreleased builds). Callers run
// the returned server on a transport, e.g.:
//
//	server, err := mcp.NewServer("1.0.0")
//	server.Run(ctx, &gosdk.StdioTransport{})
func NewServer(version string) (*gosdk.Server, error) {
	// Startup-only check: refuse to build a server whose configured audit log
	// (CUDLY_MCP_AUDIT_LOG) is unusable, so a misconfigured path surfaces here
	// instead of silently dropping every record. It does not gate individual
	// calls: a write failure later in a call only logs a warning on stderr and
	// leaves the purchase result unchanged (tools/audit.go recordPurchaseAudit).
	if err := tools.EnsureAuditLogWritable(); err != nil {
		return nil, fmt.Errorf("audit log is not readable, writable, and directory-durable: %w", err)
	}

	s := gosdk.NewServer(&gosdk.Implementation{Name: ServerName, Version: version}, nil)
	s.AddReceivingMiddleware(nullArgumentsAsEmptyObject)

	regs := registrations()
	descriptors := make([]tools.Descriptor, 0, len(regs)+1)
	for _, r := range regs {
		descriptors = append(descriptors, r.Descriptor())
	}
	// Include cudly_list_commitment_actions' own entry so the catalog it
	// returns really does list "every tool available on this MCP server" as
	// its description promises, itself included.
	descriptors = append(descriptors, tools.ListCommitmentActionsDescriptor())

	listTool := tools.NewListCommitmentActions(descriptors)
	regs = append(regs, listTool)

	for _, r := range regs {
		if err := r.Register(s); err != nil {
			return nil, fmt.Errorf("register tool %q: %w", r.Descriptor().Name, err)
		}
	}

	return s, nil
}

// nullArgumentsAsEmptyObject rewrites an explicit `"arguments": null` on
// tools/call to {} because go-sdk v1.8.0 (mcp/tool.go:98-117) unmarshals it
// into a nil map and jsonschema-go ApplyDefaults then panics, killing the server.
func nullArgumentsAsEmptyObject(next gosdk.MethodHandler) gosdk.MethodHandler {
	return func(ctx context.Context, method string, req gosdk.Request) (gosdk.Result, error) {
		if call, ok := req.(*gosdk.CallToolRequest); ok && call.Params != nil &&
			bytes.Equal(bytes.TrimSpace(call.Params.Arguments), []byte("null")) {
			call.Params.Arguments = []byte("{}")
		}
		return next(ctx, method, req)
	}
}
