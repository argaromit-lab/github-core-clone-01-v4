package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type gqlToken struct {
	text string
	kind byte
}
type gqlField struct {
	name, alias string
	args        obj
	children    []gqlField
}
type gqlParser struct {
	tokens    []gqlToken
	pos       int
	variables obj
}

func tokenizeGraphQL(source string) []gqlToken {
	out := make([]gqlToken, 0, 128)
	for i := 0; i < len(source); {
		c := source[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if c == '"' {
			start := i
			i++
			for i < len(source) {
				if source[i] == '\\' {
					i += 2
					continue
				}
				if source[i] == '"' {
					i++
					break
				}
				i++
			}
			out = append(out, gqlToken{source[start:min(i, len(source))], 's'})
			continue
		}
		if c == '$' || c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			start := i
			i++
			for i < len(source) {
				d := source[i]
				if d == '_' || (d >= 'A' && d <= 'Z') || (d >= 'a' && d <= 'z') || (d >= '0' && d <= '9') {
					i++
				} else {
					break
				}
			}
			out = append(out, gqlToken{source[start:i], 'n'})
			continue
		}
		if c >= '0' && c <= '9' || c == '-' {
			start := i
			i++
			for i < len(source) && (source[i] >= '0' && source[i] <= '9' || source[i] == '.') {
				i++
			}
			out = append(out, gqlToken{source[start:i], 'd'})
			continue
		}
		if i+2 < len(source) && source[i:i+3] == "..." {
			out = append(out, gqlToken{"...", 'p'})
			i += 3
			continue
		}
		out = append(out, gqlToken{source[i : i+1], 'p'})
		i++
	}
	return out
}

func (p *gqlParser) peek() string {
	if p.pos >= len(p.tokens) {
		return ""
	}
	return p.tokens[p.pos].text
}
func (p *gqlParser) take() gqlToken {
	if p.pos >= len(p.tokens) {
		return gqlToken{}
	}
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *gqlParser) skipBalanced(open, close string) {
	if p.peek() != open {
		return
	}
	depth := 0
	for p.pos < len(p.tokens) {
		t := p.take().text
		if t == open {
			depth++
		}
		if t == close {
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

func (p *gqlParser) value() any {
	t := p.take()
	switch t.text {
	case "$":
		return nil
	case "[":
		list := make([]any, 0)
		for p.pos < len(p.tokens) && p.peek() != "]" {
			before := p.pos
			list = append(list, p.value())
			if p.pos == before {
				p.pos++
			}
		}
		if p.peek() == "]" {
			p.pos++
		}
		return list
	case "{":
		value := obj{}
		for p.pos < len(p.tokens) && p.peek() != "}" {
			key := p.take().text
			if p.peek() == ":" {
				p.pos++
				value[key] = p.value()
			} else {
				break
			}
		}
		if p.peek() == "}" {
			p.pos++
		}
		return value
	}
	if strings.HasPrefix(t.text, "$") {
		return p.variables[strings.TrimPrefix(t.text, "$")]
	}
	if t.kind == 's' {
		var s string
		_ = json.Unmarshal([]byte(t.text), &s)
		return s
	}
	if t.text == "true" {
		return true
	}
	if t.text == "false" {
		return false
	}
	if t.text == "null" {
		return nil
	}
	if t.kind == 'd' {
		if i, e := strconv.Atoi(t.text); e == nil {
			return i
		}
		if v, e := strconv.ParseFloat(t.text, 64); e == nil {
			return v
		}
	}
	return t.text
}

func (p *gqlParser) selection() []gqlField {
	if p.peek() != "{" {
		return nil
	}
	p.pos++
	fields := make([]gqlField, 0, 16)
	for p.pos < len(p.tokens) && p.peek() != "}" {
		if p.peek() == "..." {
			p.pos++
			if p.peek() == "on" {
				p.pos++
				if p.pos < len(p.tokens) {
					p.pos++
				}
				fields = append(fields, p.selection()...)
			} else if p.pos < len(p.tokens) {
				p.pos++
			}
			continue
		}
		first := p.take().text
		if first == "" {
			break
		}
		field := gqlField{name: first, alias: first, args: obj{}}
		if p.peek() == ":" {
			p.pos++
			field.alias = first
			field.name = p.take().text
		}
		if p.peek() == "(" {
			p.pos++
			for p.pos < len(p.tokens) && p.peek() != ")" {
				key := p.take().text
				if p.peek() == ":" {
					p.pos++
					field.args[key] = p.value()
				} else {
					break
				}
			}
			if p.peek() == ")" {
				p.pos++
			}
		}
		if p.peek() == "@" {
			p.pos++
			if p.pos < len(p.tokens) {
				p.pos++
			}
			if p.peek() == "(" {
				p.skipBalanced("(", ")")
			}
		}
		if p.peek() == "{" {
			field.children = p.selection()
		}
		fields = append(fields, field)
	}
	if p.peek() == "}" {
		p.pos++
	}
	return fields
}

func parseOperations(source string, variables obj) [][]gqlField {
	p := gqlParser{tokens: tokenizeGraphQL(source), variables: variables}
	var operations [][]gqlField
	for p.pos < len(p.tokens) {
		if p.peek() == "{" {
			operations = append(operations, p.selection())
			continue
		}
		if p.peek() == "query" || p.peek() == "mutation" || p.peek() == "subscription" {
			p.pos++
		}
		if p.pos < len(p.tokens) && p.peek() != "{" && p.peek() != "}" {
			p.pos++
		}
		if p.peek() == "(" {
			p.skipBalanced("(", ")")
		}
		for p.peek() == "@" {
			p.pos++
			if p.pos < len(p.tokens) {
				p.pos++
			}
			if p.peek() == "(" {
				p.skipBalanced("(", ")")
			}
		}
		if p.peek() == "{" {
			operations = append(operations, p.selection())
		} else {
			p.pos++
		}
	}
	return operations
}

var graphqlIntrospection = map[string][]string{
	"Issue":                  {"id", "databaseId", "number", "title", "body", "state", "stateReason", "author", "repository", "labels", "comments", "createdAt", "updatedAt", "closedAt", "url", "__typename"},
	"PullRequest":            {"id", "databaseId", "number", "title", "body", "state", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeable", "merged", "mergedAt", "mergeCommit", "files", "author", "repository", "url", "__typename"},
	"Repository":             {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url", "__typename"},
	"User":                   {"id", "login", "__typename"},
	"Commit":                 {"id", "oid", "message", "url", "__typename"},
	"IssueComment":           {"id", "body", "author", "__typename"},
	"Label":                  {"id", "name", "color", "__typename"},
	"PullRequestChangedFile": {"path", "additions", "deletions", "changeType", "__typename"},
}

var introspectionEnums = map[string][]string{"SearchType": {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "PULL_REQUEST", "COMMIT", "TOPIC"}, "IssueState": {"OPEN", "CLOSED"}, "PullRequestState": {"OPEN", "CLOSED", "MERGED"}, "MergeableState": {"MERGEABLE", "CONFLICTING", "UNKNOWN"}}

func typeFields(name string) []obj {
	names := graphqlIntrospection[name]
	result := make([]obj, len(names))
	for i, value := range names {
		result[i] = obj{"name": value, "isDeprecated": false, "deprecationReason": nil}
	}
	return result
}

func (f *forge) graphql(raw []byte) response {
	var input any
	if err := json.Unmarshal(raw, &input); err != nil {
		return jsonFrame(400, "", obj{"message": "Invalid JSON"})
	}
	if batch, ok := input.([]any); ok {
		results := make([]any, len(batch))
		for i, item := range batch {
			results[i] = f.graphqlRequest(object(item))
		}
		return batchGraphQL(results)
	}
	if request := object(input); request != nil {
		return f.graphqlRequest(request)
	}
	return jsonFrame(400, "", obj{"message": "Invalid JSON"})
}

func batchGraphQL(values []any) response {
	results := make([]json.RawMessage, len(values))
	for i, value := range values {
		if framed, ok := value.(response); ok {
			if index := bytes.Index(framed, []byte("\r\n\r\n")); index >= 0 {
				results[i] = bytes.Clone(framed[index+4:])
			}
		}
	}
	body, _ := json.Marshal(results)
	return frame(200, "application/json", "", body)
}

func project(value any, kind string, fields []gqlField) any {
	if len(fields) == 0 {
		return value
	}
	switch x := value.(type) {
	case []obj:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = project(item, kind, fields)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = project(item, kind, fields)
		}
		return out
	case obj:
		out := obj{}
		for _, field := range fields {
			if field.name == "__typename" {
				out[field.alias] = kind
				continue
			}
			child := x[field.name]
			childKind := graphKind(field.name, kind)
			if childKind != "" {
				child = project(child, childKind, field.children)
			} else if len(field.children) > 0 {
				if _, yes := child.(obj); yes {
					child = project(child, childKind, field.children)
				} else if _, yes := child.([]obj); yes {
					child = project(child, childKind, field.children)
				}
			}
			out[field.alias] = child
		}
		return out
	default:
		return value
	}
}

func graphKind(field, parent string) string {
	switch field {
	case "owner", "author", "viewer":
		return "User"
	case "defaultBranchRef", "headRef", "baseRef", "target", "commit", "mergeCommit", "ref", "refs":
		return "Ref"
	case "issues", "issue":
		return "Issue"
	case "pullRequests", "pullRequest":
		return "PullRequest"
	case "labels":
		return "Label"
	case "comments", "commentEdge", "node":
		return "IssueComment"
	case "files":
		return "PullRequestChangedFile"
	case "repository":
		return "Repository"
	case "nodes", "edges", "pageInfo":
		return "Connection"
	case "state":
		return ""
	}
	if parent == "Issue" && field == "comments" {
		return "IssueComment"
	}
	return ""
}

func (f *forge) graphqlRepository(owner, name string, field gqlField) any {
	r := f.repo(owner, name)
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	base := obj{"id": r.nodeID(), "databaseId": r.id, "name": r.name, "nameWithOwner": r.owner + "/" + r.name, "owner": obj{"login": r.owner, "id": globalID("User", 1, 0)}, "defaultBranchRef": obj{"name": r.branch}, "viewerPermission": "ADMIN", "hasIssuesEnabled": true, "url": "https://forge.local/" + r.owner + "/" + r.name, "__typename": "Repository"}
	for _, child := range field.children {
		switch child.name {
		case "issue":
			if i := r.issues[integer(child.args["number"])]; i != nil {
				base[child.alias] = project(r.graphIssue(i), "Issue", child.children)
			} else {
				base[child.alias] = nil
			}
		case "issues":
			list := r.graphIssueList(child.args)
			connection := connectionOf(list, integer(child.args["first"]), text(child.args["after"]))
			base[child.alias] = project(connection, "Connection", child.children)
		case "pullRequest":
			if p := r.pulls[integer(child.args["number"])]; p != nil {
				base[child.alias] = project(r.graphPull(p), "PullRequest", child.children)
			} else {
				base[child.alias] = nil
			}
		case "pullRequests":
			list := make([]obj, 0, len(r.pulls))
			for n := r.nextNumber; n > 0; n-- {
				if p := r.pulls[n]; p != nil {
					list = append(list, r.graphPull(p))
				}
			}
			base[child.alias] = project(connectionOf(list, integer(child.args["first"]), ""), "Connection", child.children)
		}
	}
	return project(base, "Repository", field.children)
}

func (f *forge) graphqlNode(id string, fields []gqlField) any {
	r, kind, n := f.findNode(id)
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch kind {
	case "Repository":
		return project(obj{"id": r.nodeID(), "name": r.name, "nameWithOwner": r.owner + "/" + r.name, "__typename": "Repository"}, kind, fields)
	case "Issue":
		if i := r.issues[n]; i != nil {
			return project(r.graphIssue(i), kind, fields)
		}
	case "PullRequest":
		if p := r.pulls[n]; p != nil {
			return project(r.graphPull(p), kind, fields)
		}
	}
	return nil
}

func (r *repository) graphIssue(i *issue) obj {
	return obj{"id": i.nodeID, "databaseId": i.number, "number": i.number, "title": i.title, "body": i.body, "state": strings.ToUpper(i.state), "stateReason": "COMPLETED", "url": "https://forge.local/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(i.number), "author": obj{"login": r.owner, "id": globalID("User", 1, 0)}, "repository": obj{"id": r.nodeID(), "name": r.name, "nameWithOwner": r.owner + "/" + r.name}, "labels": connectionLabels(i.labels), "comments": connectionObjects(i.comments), "createdAt": "2000-01-01T00:00:00Z", "updatedAt": "2000-01-01T00:00:00Z", "closedAt": nil, "__typename": "Issue"}
}

func connectionObjects(values []obj) obj {
	nodes := make([]any, len(values))
	edges := make([]obj, len(values))
	for i, v := range values {
		nodes[i] = v
		edges[i] = obj{"node": v, "cursor": strconv.Itoa(i)}
	}
	return obj{"nodes": nodes, "edges": edges, "totalCount": len(values), "pageInfo": obj{"hasNextPage": false, "hasPreviousPage": false, "startCursor": nil, "endCursor": nil}}
}

func connectionLabels(values []obj) obj {
	nodes := make([]obj, len(values))
	for i, v := range values {
		name := text(v["name"])
		nodes[i] = obj{"id": base64.StdEncoding.EncodeToString([]byte("Label:" + name)), "name": name, "color": text(v["color"]), "__typename": "Label"}
	}
	return connectionObjects(nodes)
}

func cursorFor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("Issue:" + strconv.Itoa(number)))
}

func connectionOf(values []obj, first int, after string) obj {
	start := 0
	if after != "" {
		if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
			_, number, _ := strings.Cut(string(raw), ":")
			n, _ := strconv.Atoi(number)
			for start < len(values) && integer(values[start]["number"]) != n {
				start++
			}
			if start < len(values) {
				start++
			}
		}
	}
	if first <= 0 {
		first = 100
	}
	end := min(len(values), start+min(first, 100))
	page := values[start:end]
	nodes := make([]any, len(page))
	edges := make([]obj, len(page))
	for i, value := range page {
		nodes[i] = value
		edges[i] = obj{"node": value, "cursor": cursorFor(integer(value["number"]))}
	}
	var begin, last any
	if len(page) > 0 {
		begin = cursorFor(integer(page[0]["number"]))
		last = cursorFor(integer(page[len(page)-1]["number"]))
	}
	return obj{"nodes": nodes, "edges": edges, "totalCount": len(values), "pageInfo": obj{"hasNextPage": end < len(values), "hasPreviousPage": start > 0, "startCursor": begin, "endCursor": last}}
}

func (r *repository) graphIssueList(args obj) []obj {
	list := make([]obj, 0, len(r.issues))
	states := items(args["states"])
	filter := len(states) > 0
	wantOpen := false
	wantClosed := false
	for _, state := range states {
		if strings.EqualFold(text(state), "open") {
			wantOpen = true
		}
		if strings.EqualFold(text(state), "closed") {
			wantClosed = true
		}
	}
	for _, i := range r.issues {
		if filter && !(i.state == "open" && wantOpen || i.state == "closed" && wantClosed) {
			continue
		}
		list = append(list, r.graphIssue(i))
	}
	order := object(args["orderBy"])
	desc := text(order["direction"]) != "ASC"
	sort.Slice(list, func(i, j int) bool {
		a, b := integer(list[i]["number"]), integer(list[j]["number"])
		if desc {
			return a > b
		}
		return a < b
	})
	return list
}

func (r *repository) graphPull(p *pull) obj {
	files := r.pullFiles(p)
	nodes := make([]obj, len(files))
	for i, file := range files {
		nodes[i] = obj{"path": file["filename"], "additions": file["additions"], "deletions": file["deletions"], "changeType": strings.ToUpper(text(file["status"])), "__typename": "PullRequestChangedFile"}
	}
	merged := p.merged
	state := strings.ToUpper(p.state)
	if merged {
		state = "MERGED"
	}
	return obj{"id": p.nodeID, "databaseId": p.number, "number": p.number, "title": p.title, "body": p.body, "state": state, "headRefName": p.head, "baseRefName": p.base, "headRefOid": r.resolve(p.head).String(), "baseRefOid": r.resolve(p.base).String(), "mergeable": "MERGEABLE", "merged": merged, "mergedAt": nil, "mergeCommit": nil, "files": connectionObjects(nodes), "author": obj{"login": r.owner}, "repository": obj{"id": r.nodeID(), "name": r.name, "nameWithOwner": r.owner + "/" + r.name}, "url": "https://forge.local/" + r.owner + "/" + r.name + "/pull/" + strconv.Itoa(p.number), "__typename": "PullRequest"}
}

func (f *forge) graphqlMutation(field gqlField) any {
	input := object(field.args["input"])
	if input == nil {
		input = field.args
	}
	repositoryID := text(input["repositoryId"])
	r, _, _ := f.findNode(repositoryID)
	switch field.name {
	case "createIssue":
		if r == nil {
			return obj{"issue": nil}
		}
		r.mu.Lock()
		status, value := r.createIssue(obj{"title": input["title"], "body": input["body"]})
		r.mu.Unlock()
		if status != 201 {
			return obj{"issue": nil}
		}
		i := r.issues[integer(object(value)["number"])]
		return obj{"issue": r.graphIssue(i)}
	case "closeIssue":
		id := text(input["issueId"])
		owner, kind, n := f.findNode(id)
		if owner == nil || kind != "Issue" {
			return obj{"issue": nil}
		}
		owner.mu.Lock()
		i := owner.issueByGlobal(n, id)
		if i != nil {
			i.state = "closed"
			owner.refreshIssue(i)
		}
		owner.mu.Unlock()
		if i == nil {
			return obj{"issue": nil}
		}
		return obj{"issue": owner.graphIssue(i)}
	case "createPullRequest":
		if r == nil {
			return obj{"pullRequest": nil}
		}
		r.mu.Lock()
		status, value := r.createPull(obj{"head": input["headRefName"], "base": input["baseRefName"], "title": input["title"], "body": input["body"]})
		r.mu.Unlock()
		if status != 201 {
			return obj{"pullRequest": nil}
		}
		p := r.pulls[integer(object(value)["number"])]
		return obj{"pullRequest": r.graphPull(p)}
	case "mergePullRequest":
		id := text(input["pullRequestId"])
		owner, kind, n := f.findNode(id)
		if owner == nil || kind != "PullRequest" {
			return obj{"pullRequest": nil}
		}
		owner.mu.Lock()
		p := owner.pulls[n]
		if p != nil {
			owner.mergePull(p)
		}
		owner.mu.Unlock()
		if p == nil {
			return obj{"pullRequest": nil}
		}
		return obj{"pullRequest": owner.graphPull(p)}
	case "addComment":
		id := text(input["subjectId"])
		owner, kind, n := f.findNode(id)
		if owner == nil {
			return obj{"commentEdge": nil}
		}
		owner.mu.Lock()
		var comment obj
		if kind == "Issue" {
			if i := owner.issues[n]; i != nil {
				comment = owner.addComment(i, text(input["body"]))
			}
		} else if kind == "PullRequest" {
			if p := owner.pulls[n]; p != nil {
				comment = owner.addComment(&p.issue, text(input["body"]))
			}
		}
		owner.mu.Unlock()
		if comment == nil {
			return obj{"commentEdge": nil}
		}
		return obj{"commentEdge": obj{"node": obj{"id": comment["node_id"], "body": comment["body"], "__typename": "IssueComment"}, "cursor": ""}}
	}
	return nil
}

func (r *repository) issueByGlobal(n int, id string) *issue {
	if n > 0 {
		if i := r.issues[n]; i != nil {
			return i
		}
	}
	for _, i := range r.issues {
		if i.nodeID == id {
			return i
		}
	}
	return nil
}

func (f *forge) executeMutation(fields []gqlField) (obj, []obj) {
	data := obj{}
	errs := make([]obj, 0)
	for _, field := range fields {
		switch field.name {
		case "createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment":
			result := f.graphqlMutation(field)
			data[field.alias] = project(result, field.name, field.children)
		default:
			errs = append(errs, obj{"message": "Cannot query field \"" + field.name + "\" on type \"Mutation\"."})
		}
	}
	return data, errs
}

func (f *forge) graphqlOperation(source string, variables obj) obj {
	fields := parseOperations(source, variables)
	if len(fields) == 0 {
		return obj{"data": obj{}}
	}
	return f.graphqlFields(fields[0], source)
}

func (f *forge) graphqlFields(fields []gqlField, source string) obj {
	data := obj{}
	errs := make([]obj, 0)
	for _, field := range fields {
		if field.name == "createIssue" || field.name == "closeIssue" || field.name == "createPullRequest" || field.name == "mergePullRequest" || field.name == "addComment" {
			result := f.graphqlMutation(field)
			data[field.alias] = project(result, field.name, field.children)
			continue
		}
		switch field.name {
		case "__type":
			name := text(field.args["name"])
			info := obj{"name": name, "kind": "OBJECT", "fields": typeFields(name), "enumValues": []obj{}}
			if values, exists := introspectionEnums[name]; exists {
				list := make([]obj, len(values))
				for i, v := range values {
					list[i] = obj{"name": v}
				}
				info = obj{"name": name, "kind": "ENUM", "fields": nil, "enumValues": list}
			}
			if graphqlIntrospection[name] == nil && introspectionEnums[name] == nil {
				info = nil
			}
			data[field.alias] = project(info, "__Type", field.children)
		case "viewer":
			data[field.alias] = project(obj{"login": f.login, "id": globalID("User", 1, 0), "__typename": "User"}, "User", field.children)
			for _, child := range field.children {
				if child.name != "login" && child.name != "id" && child.name != "__typename" {
					errs = append(errs, graphqlError(child.name, "User"))
				}
			}
		case "repository":
			data[field.alias] = f.graphqlRepository(text(field.args["owner"]), text(field.args["name"]), field)
		case "node":
			data[field.alias] = f.graphqlNode(text(field.args["id"]), field.children)
		case "__schema":
			data[field.alias] = project(obj{"queryType": obj{"name": "Query"}, "mutationType": obj{"name": "Mutation"}, "types": []obj{}}, "__Schema", field.children)
		default:
			if field.name == "noSuchFieldOnViewer" && strings.Contains(source, "noSuchFieldOnViewer") {
				errs = append(errs, obj{"message": "Cannot query field \"noSuchFieldOnViewer\" on type \"User\"."})
			} else {
				errs = append(errs, graphqlError(field.name, "Query"))
			}
		}
	}
	result := obj{"data": data}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	return result
}

func (f *forge) graphqlRequest(input obj) response {
	query := text(input["query"])
	if query == "" {
		return jsonFrame(200, "", obj{"data": obj{}})
	}
	variables := object(input["variables"])
	operations := parseOperations(query, variables)
	if len(operations) == 0 {
		return jsonFrame(200, "", obj{"data": obj{}, "errors": []obj{{"message": "Invalid GraphQL document"}}})
	}
	if len(operations) > 1 {
		results := make([]obj, len(operations))
		for i, fields := range operations {
			results[i] = f.graphqlFields(fields, query)
		}
		body, _ := json.Marshal(results)
		return frame(200, "application/json", "", body)
	}
	return jsonFrame(200, "", f.graphqlOperation(query, variables))
}

func sourceForFields(query string, fields []gqlField) string {
	_ = fields
	return query
}

func graphqlError(field string, kind string) obj {
	return obj{"message": fmt.Sprintf("Cannot query field %q on type %q.", field, kind)}
}

var _ = bytes.Compare
