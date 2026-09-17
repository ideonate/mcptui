package mcp

import (
	"context"
	"encoding/json"
)

// maxPages guards against servers that return the same cursor forever.
const maxPages = 1000

// listAll follows nextCursor until exhausted, decoding items under key.
func listAll[T any](ctx context.Context, c *Client, method, key string) ([]T, []*Exchange, error) {
	var (
		items []T
		exs   []*Exchange
		cur   string
		seen  = map[string]bool{}
	)
	for page := 0; page < maxPages; page++ {
		var params any
		if cur != "" {
			params = map[string]any{"cursor": cur}
		}
		ex := c.Request(ctx, method, params, nil)
		exs = append(exs, ex)
		if ex.Err != nil {
			return items, exs, ex.Err
		}
		var res map[string]json.RawMessage
		if err := json.Unmarshal(ex.Result, &res); err != nil {
			return items, exs, err
		}
		var pageItems []T
		if raw, ok := res[key]; ok {
			if err := json.Unmarshal(raw, &pageItems); err != nil {
				return items, exs, err
			}
		}
		items = append(items, pageItems...)
		var next string
		if raw, ok := res["nextCursor"]; ok {
			_ = json.Unmarshal(raw, &next)
		}
		if next == "" || seen[next] {
			break
		}
		seen[next] = true
		cur = next
	}
	return items, exs, nil
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, []*Exchange, error) {
	return listAll[Tool](ctx, c, "tools/list", "tools")
}

func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, []*Exchange, error) {
	return listAll[Prompt](ctx, c, "prompts/list", "prompts")
}

func (c *Client) ListResources(ctx context.Context) ([]Resource, []*Exchange, error) {
	return listAll[Resource](ctx, c, "resources/list", "resources")
}

func (c *Client) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, []*Exchange, error) {
	return listAll[ResourceTemplate](ctx, c, "resources/templates/list", "resourceTemplates")
}

// CallTool invokes tools/call. args may be nil or a JSON object.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage, opts *RequestOptions) (*CallToolResult, *Exchange) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	} else {
		params["arguments"] = map[string]any{}
	}
	ex := c.Request(ctx, "tools/call", params, opts)
	var res CallToolResult
	if err := ex.Decode(&res); err != nil {
		if ex.Err == nil {
			ex.Err = err
		}
		return nil, ex
	}
	return &res, ex
}

// GetPrompt invokes prompts/get.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, *Exchange) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	ex := c.Request(ctx, "prompts/get", params, nil)
	var res GetPromptResult
	if err := ex.Decode(&res); err != nil {
		if ex.Err == nil {
			ex.Err = err
		}
		return nil, ex
	}
	return &res, ex
}

// ReadResource invokes resources/read.
func (c *Client) ReadResource(ctx context.Context, uri string) (*ReadResourceResult, *Exchange) {
	ex := c.Request(ctx, "resources/read", map[string]any{"uri": uri}, nil)
	var res ReadResourceResult
	if err := ex.Decode(&res); err != nil {
		if ex.Err == nil {
			ex.Err = err
		}
		return nil, ex
	}
	return &res, ex
}

func (c *Client) Subscribe(ctx context.Context, uri string) *Exchange {
	return c.Request(ctx, "resources/subscribe", map[string]any{"uri": uri}, nil)
}

func (c *Client) Unsubscribe(ctx context.Context, uri string) *Exchange {
	return c.Request(ctx, "resources/unsubscribe", map[string]any{"uri": uri}, nil)
}

func (c *Client) Ping(ctx context.Context) *Exchange {
	return c.Request(ctx, "ping", nil, nil)
}

// SetLogLevel invokes logging/setLevel.
func (c *Client) SetLogLevel(ctx context.Context, level string) *Exchange {
	return c.Request(ctx, "logging/setLevel", map[string]any{"level": level}, nil)
}

// CompleteRef identifies what is being completed.
type CompleteRef struct {
	Type string `json:"type"` // "ref/prompt" or "ref/resource"
	Name string `json:"name,omitempty"`
	URI  string `json:"uri,omitempty"`
}

// Complete invokes completion/complete.
func (c *Client) Complete(ctx context.Context, ref CompleteRef, argName, value string, context map[string]string) (*CompleteResult, *Exchange) {
	params := map[string]any{
		"ref":      ref,
		"argument": map[string]any{"name": argName, "value": value},
	}
	if len(context) > 0 {
		params["context"] = map[string]any{"arguments": context}
	}
	ex := c.Request(ctx, "completion/complete", params, nil)
	var res CompleteResult
	if err := ex.Decode(&res); err != nil {
		if ex.Err == nil {
			ex.Err = err
		}
		return nil, ex
	}
	return &res, ex
}
