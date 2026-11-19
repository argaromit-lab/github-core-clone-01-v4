package main

import (
	"encoding/json"
	"net/url"
	"strings"
)

const mcpVersion = "2026-07-28"

type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema obj    `json:"inputSchema"`
}

var mcpTools = []mcpTool{
	{"create_repo", "Create a repository owned by the authenticated user.", obj{"type": "object", "properties": obj{"name": obj{"type": "string"}, "description": obj{"type": "string"}, "private": obj{"type": "boolean"}}, "required": []string{"name"}}},
	{"get_repo", "Read a repository by owner and name.", obj{"type": "object", "properties": obj{"owner": obj{"type": "string"}, "repo": obj{"type": "string"}}, "required": []string{"owner", "repo"}}},
	{"create_issue", "Create an issue in a repository.", obj{"type": "object", "properties": obj{"owner": obj{"type": "string"}, "repo": obj{"type": "string"}, "title": obj{"type": "string"}, "body": obj{"type": "string"}}, "required": []string{"owner", "repo", "title"}}},
	{"list_issues", "List repository issues, optionally filtered by state.", obj{"type": "object", "properties": obj{"owner": obj{"type": "string"}, "repo": obj{"type": "string"}, "state": obj{"type": "string"}}, "required": []string{"owner", "repo"}}},
	{"create_pull_request", "Create a pull request between branches.", obj{"type": "object", "properties": obj{"owner": obj{"type": "string"}, "repo": obj{"type": "string"}, "title": obj{"type": "string"}, "body": obj{"type": "string"}, "head": obj{"type": "string"}, "base": obj{"type": "string"}}, "required": []string{"owner", "repo", "title", "head", "base"}}},
	{"merge_pull_request", "Merge a pull request into its base branch.", obj{"type": "object", "properties": obj{"owner": obj{"type": "string"}, "repo": obj{"type": "string"}, "pull_number": obj{"type": "integer"}}, "required": []string{"owner", "repo", "pull_number"}}},
	{"create_label", "Create a repository label.", obj{"type": "object", "properties": obj{"owner": obj{"type": "string"}, "repo": obj{"type": "string"}, "name": obj{"type": "string"}, "color": obj{"type": "string"}}, "required": []string{"owner", "repo", "name"}}},
	{"list_branches", "List repository branches.", obj{"type": "object", "properties": obj{"owner": obj{"type": "string"}, "repo": obj{"type": "string"}}, "required": []string{"owner", "repo"}}},
}

func rpcError(id json.RawMessage, code int, message string) response {
	return rpcFrame(200, id, code, message)
}

func rpcFrame(status int, id json.RawMessage, code int, message string) response {
	return jsonFrame(status, "", obj{"jsonrpc": "2.0", "id": id, "error": obj{"code": code, "message": message}})
}

func (f *forge) mcp(raw []byte) response {
	var request struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  obj             `json:"params"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return rpcFrame(400, nil, -32700, "Parse error")
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return rpcError(nil, -32600, "Invalid request")
	}
	if len(request.ID) == 0 {
		return frame(202, "application/json", "", nil)
	}
	var result any
	switch request.Method {
	case "initialize":
		requested := text(request.Params["protocolVersion"])
		version := mcpVersion
		if requested != "" && requested <= mcpVersion {
			version = requested
		}
		result = obj{"protocolVersion": version, "capabilities": obj{"tools": obj{"listChanged": false}}, "serverInfo": obj{"name": "static-git-forge", "version": "2.0.0"}}
	case "server/discover":
		result = obj{"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}, "serverInfo": obj{"name": "static-git-forge", "version": "2.0.0"}, "capabilities": obj{"tools": obj{"listChanged": false}}}
	case "ping":
		result = obj{}
	case "tools/list":
		result = obj{"tools": mcpTools, "nextCursor": nil}
	case "tools/call":
		name := text(request.Params["name"])
		arguments := object(request.Params["arguments"])
		if arguments == nil {
			arguments = obj{}
		}
		var tool *mcpTool
		for i := range mcpTools {
			if strings.EqualFold(name, mcpTools[i].Name) || strings.HasSuffix(strings.ToLower(name), "__"+mcpTools[i].Name) {
				tool = &mcpTools[i]
				break
			}
		}
		if tool == nil {
			return rpcError(request.ID, -32601, "Unknown tool: "+name)
		}
		for _, required := range items(tool.InputSchema["required"]) {
			key := text(required)
			if _, ok := arguments[key]; !ok {
				return rpcError(request.ID, -32602, "Missing required argument: "+key)
			}
		}
		value, isError := f.callTool(tool.Name, arguments)
		encoded, _ := json.Marshal(value)
		result = obj{"content": []obj{{"type": "text", "text": string(encoded)}}, "structuredContent": value, "isError": isError}
	default:
		return rpcError(request.ID, -32601, "Method not found: "+request.Method)
	}
	out := obj{"jsonrpc": "2.0", "id": request.ID, "result": result}
	return jsonFrame(200, "", out)
}

func (f *forge) callTool(name string, args obj) (any, bool) {
	owner, repo := text(args["owner"]), text(args["repo"])
	if name == "create_repo" {
		if full := text(args["full_name"]); full != "" {
			owner, repo, _ = strings.Cut(full, "/")
		}
		input := obj{"name": text(args["name"]), "description": text(args["description"]), "private": args["private"]}
		status, value := f.createRepo(input)
		return value, status >= 400
	}
	r := f.repo(owner, repo)
	if r == nil {
		return obj{"message": "Not Found"}, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.invalidateWire()
	switch name {
	case "get_repo":
		return r.view(), false
	case "create_issue":
		status, value := r.createIssue(args)
		return value, status != 201
	case "list_issues":
		values := urlValuesState(text(args["state"]))
		return r.issueList(values), false
	case "create_pull_request":
		status, value := r.createPull(args)
		return value, status != 201
	case "merge_pull_request":
		p := r.pulls[integer(args["pull_number"])]
		if p == nil {
			return obj{"message": "Not Found"}, true
		}
		status, value := r.mergePull(p)
		return obj{"pull": value, "merged": status == 200}, status != 200
	case "create_label":
		name := text(args["name"])
		label := obj{"name": name, "color": text(args["color"])}
		r.labels[name] = label
		return label, name == ""
	case "list_branches":
		list := make([]obj, 0)
		for _, ref := range r.refNames() {
			if strings.HasPrefix(ref, "refs/heads/") {
				list = append(list, obj{"name": strings.TrimPrefix(ref, "refs/heads/"), "commit": obj{"sha": r.refs[ref].String()}})
			}
		}
		return list, false
	}
	return obj{"message": "Not Found"}, true
}

func urlValuesState(state string) url.Values { return url.Values{"state": []string{state}} }
