package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const jsonRPCVersion = "2.0"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve handles Language Server Protocol messages until the client sends exit
// or the input stream reaches EOF.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		body, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		var message request
		if err := json.Unmarshal(body, &message); err != nil {
			return fmt.Errorf("decode JSON-RPC message: %w", err)
		}
		if message.Method == "exit" {
			return nil
		}
		if len(message.ID) == 0 {
			continue
		}

		var reply response
		switch message.Method {
		case "initialize":
			reply = response{
				JSONRPC: jsonRPCVersion,
				ID:      message.ID,
				Result: map[string]any{
					"capabilities": map[string]any{
						"textDocumentSync": 1,
						"codeLensProvider": map[string]any{
							"resolveProvider": false,
						},
					},
					"serverInfo": map[string]any{
						"name":    "go-implementation-lens-ls",
						"version": "0.0.1-poc",
					},
				},
			}
		case "textDocument/codeLens":
			result, err := staticCodeLens(message.Params)
			if err != nil {
				reply = response{
					JSONRPC: jsonRPCVersion,
					ID:      message.ID,
					Error: &responseError{
						Code:    -32602,
						Message: err.Error(),
					},
				}
			} else {
				reply = response{
					JSONRPC: jsonRPCVersion,
					ID:      message.ID,
					Result:  result,
				}
			}
		case "shutdown":
			reply = response{
				JSONRPC: jsonRPCVersion,
				ID:      message.ID,
				Result:  json.RawMessage("null"),
			}
		default:
			reply = response{
				JSONRPC: jsonRPCVersion,
				ID:      message.ID,
				Error: &responseError{
					Code:    -32601,
					Message: "method not found: " + message.Method,
				},
			}
		}

		if err := writeFrame(out, reply); err != nil {
			return err
		}
	}
}

func staticCodeLens(params json.RawMessage) ([]any, error) {
	var input struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, fmt.Errorf("invalid textDocument/codeLens params: %w", err)
	}
	if input.TextDocument.URI == "" {
		return nil, errors.New("textDocument/codeLens params must include textDocument.uri")
	}

	position := map[string]any{"line": 0, "character": 0}
	rangeValue := map[string]any{"start": position, "end": position}
	location := map[string]any{
		"uri":   input.TextDocument.URI,
		"range": rangeValue,
	}

	return []any{
		map[string]any{
			"range": rangeValue,
			"command": map[string]any{
				"title":   "Go Implementation Lens: POC",
				"command": "editor.action.showReferences",
				"arguments": []any{
					input.TextDocument.URI,
					position,
					[]any{location},
				},
			},
		},
	}, nil
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}

		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		contentLength, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil || contentLength < 0 {
			return nil, fmt.Errorf("invalid Content-Length header %q", value)
		}
	}
	if contentLength < 0 {
		return nil, errors.New("missing Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read JSON-RPC body: %w", err)
	}
	return body, nil
}

func writeFrame(out io.Writer, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode JSON-RPC response: %w", err)
	}
	if _, err := fmt.Fprintf(out, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return fmt.Errorf("write JSON-RPC header: %w", err)
	}
	if _, err := out.Write(body); err != nil {
		return fmt.Errorf("write JSON-RPC body: %w", err)
	}
	return nil
}
