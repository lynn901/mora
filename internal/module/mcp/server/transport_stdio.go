package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lynn901/mora/internal/module/mcp/auth"
)

// RunStdio runs the MCP server over the stdio transport: each line on stdin is
// one JSON-RPC 2.0 request, each response is written as one line to stdout
// (design doc 06 §2.3). Auth is session-level fixed: a token resolved from the
// --api-token flag / env is used for the whole stdio session.
//
// stdio is P2 (design doc 06 §2.1); HTTP/SSE is the MVP transport. This
// implementation is provided for local CLI/IDE Agent integration.
func (s *Server) RunStdio(ctx context.Context, tokenRecord *auth.TokenRecord, in io.Reader, out io.Writer) error {
	ac := tokenRecordToAuth(tokenRecord)
	reader := bufio.NewReader(in)
	encoder := json.NewEncoder(out)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		// Batch?
		if line[0] == '[' {
			var reqs []*Request
			if err := json.Unmarshal(line, &reqs); err != nil {
				_ = encoder.Encode(errorResponse(nil, ErrCodeParseError, "invalid batch"))
				continue
			}
			resps := make([]*Response, 0, len(reqs))
			for _, req := range reqs {
				resp, _ := s.Handle(auth.WithAuthContext(ctx, ac), req)
				if resp != nil {
					resps = append(resps, resp)
				}
			}
			if len(resps) > 0 {
				_ = encoder.Encode(resps)
			}
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = encoder.Encode(errorResponse(nil, ErrCodeParseError, "invalid request"))
			continue
		}
		resp, _ := s.Handle(auth.WithAuthContext(ctx, ac), &req)
		if resp != nil {
			if err := encoder.Encode(resp); err != nil {
				return fmt.Errorf("encode response: %w", err)
			}
		}
	}
}

// tokenRecordToAuth converts a resolved token into an AuthContext for the stdio
// session. If the record is nil (no token), an unauthenticated context is used
// and the server will reject protected calls.
func tokenRecordToAuth(t *auth.TokenRecord) *auth.AuthContext {
	if t == nil {
		return nil
	}
	return &auth.AuthContext{
		TokenID:      t.ID,
		TokenName:    t.Name,
		TokenPrefix:  t.Prefix,
		IdentityType: t.IdentityType,
		IdentityID:   t.IdentityID,
		IdentityName: t.IdentityName,
		Scope:        t.Scope,
		Groups:       t.Groups,
	}
}

// DefaultStdioBindings returns os.Stdin/os.Stdout for the RunStdio signature.
func DefaultStdioBindings() (io.Reader, io.Writer) {
	return os.Stdin, os.Stdout
}
