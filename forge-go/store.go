package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type forge struct {
	token, login string
	mu           sync.RWMutex
	repos        map[string]*repository
	nextID       atomic.Int64
	revision     atomic.Uint64
}

type repository struct {
	mu                               sync.RWMutex
	id                               int64
	name, owner, branch, description string
	private                          bool
	objects                          map[oid]*gitObject
	refs                             map[string]oid
	labels                           map[string]obj
	issues                           map[int]*issue
	pulls                            map[int]*pull
	nextNumber, nextComment          int
	shallow                          atomic.Pointer[cloneFrame]
	frames                           atomic.Pointer[repoFrames]
	readIndex                        sync.Map
}

type repoFrames struct {
	values map[string]response
}

func (r *repository) cachedFrame(key string) response {
	if cached := r.frames.Load(); cached != nil {
		return cached.values[key]
	}
	return nil
}

func (r *repository) storeFrame(key string, value response) {
	for {
		cached := r.frames.Load()
		values := make(map[string]response)
		if cached != nil {
			for k, v := range cached.values {
				values[k] = v
			}
		}
		values[key] = value
		if r.frames.CompareAndSwap(cached, &repoFrames{values: values}) {
			return
		}
	}
}

func (r *repository) invalidateFrames() { r.frames.Store(nil) }

type issue struct {
	id, nodeID, title, body, state string
	number                         int
	labels                         []obj
	comments                       []obj
	view                           obj
	read                           response
}

type pull struct {
	issue
	head, base                      string
	headID, baseID, forkID, mergeID oid
	merged                          bool
}

func newForge() *forge {
	login := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if login == "" {
		login = "adaiversen"
	}
	return &forge{token: os.Getenv("MAKEFUN_SHOP_TOKEN"), login: login, repos: map[string]*repository{}}
}

func (f *forge) repo(owner, name string) *repository {
	if !strings.EqualFold(owner, f.login) {
		return nil
	}
	f.mu.RLock()
	r := f.repos[strings.ToLower(name)]
	f.mu.RUnlock()
	return r
}

func globalID(kind string, repoID int64, number int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strconv.FormatInt(repoID, 10) + ":" + strconv.Itoa(number)))
}

func (r *repository) nodeID() string { return globalID("Repository", r.id, 0) }

func (r *repository) view() obj {
	return obj{"id": r.id, "node_id": r.nodeID(), "name": r.name, "full_name": r.owner + "/" + r.name, "owner": obj{"login": r.owner, "id": 1}, "private": r.private, "description": r.description, "default_branch": r.branch, "has_issues": true, "html_url": "https://forge.local/" + r.owner + "/" + r.name}
}

func (f *forge) createRepo(input obj) (int, any) {
	name := text(input["name"])
	if name == "" || len(name) > 200 || strings.ContainsAny(name, "/\\\x00\r\n") || name == "." || name == ".." {
		return 422, obj{"message": "Invalid repository name"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.ToLower(name)
	if f.repos[key] != nil {
		return 422, obj{"message": "Repository already exists"}
	}
	branch := text(input["default_branch"])
	if branch == "" {
		branch = "main"
	}
	if !validRef("refs/heads/" + branch) {
		return 422, obj{"message": "Invalid branch"}
	}
	private, _ := input["private"].(bool)
	r := &repository{id: f.nextID.Add(1), name: name, owner: f.login, branch: branch, description: text(input["description"]), private: private, objects: map[oid]*gitObject{}, refs: map[string]oid{}, labels: map[string]obj{}, issues: map[int]*issue{}, pulls: map[int]*pull{}}
	if input["auto_init"] == true {
		blob := r.saveObject(blobKind, []byte("# "+name+"\n"))
		id := r.commitFiles(map[string]treeEntry{"README.md": {"README.md", "100644", blob}}, zero, "Initial commit", f.login)
		r.refs["refs/heads/"+branch] = id
	}
	f.repos[key] = r
	return 201, r.view()
}

type pageWindow struct{ start, end, page, perPage int }

func window(values url.Values, count int) pageWindow {
	page, perPage := 1, 30
	if n, err := strconv.Atoi(values.Get("page")); err == nil && n > 0 && n <= 1000000 {
		page = n
	}
	if n, err := strconv.Atoi(values.Get("per_page")); err == nil && n > 0 {
		perPage = min(n, 100)
	}
	start := min((page-1)*perPage, count)
	return pageWindow{start, min(start+perPage, count), page, perPage}
}

func paginate(q request, values url.Values, data []obj) response {
	w := window(values, len(data))
	headers := ""
	if w.end < len(data) {
		values.Set("page", strconv.Itoa(w.page+1))
		headers = "Link: <http://" + q.host + q.path + "?" + values.Encode() + ">; rel=\"next\"\r\n"
	}
	return jsonFrame(200, headers, data[w.start:w.end])
}

func (f *forge) rest(q request) response {
	input, valid := getBody(q.body)
	if !valid {
		return badRequest
	}
	p := strings.TrimPrefix(q.path, "/api/v3")
	switch p {
	case "", "/":
		if q.method == "GET" {
			return jsonFrame(200, "X-OAuth-Scopes: repo, read:org\r\n", obj{"current_user_url": "/api/v3/user", "repository_url": "/api/v3/repos/{owner}/{repo}"})
		}
	case "/meta":
		if q.method == "GET" {
			return jsonFrame(200, "", obj{"installed_version": "static-forge-2"})
		}
	case "/user":
		if q.method == "GET" {
			return jsonFrame(200, "", obj{"login": f.login, "id": 1, "node_id": globalID("User", 1, 0)})
		}
	case "/state.json":
		if q.method == "GET" {
			return f.exportState()
		}
	case "/user/repos":
		switch q.method {
		case "POST":
			status, value := f.createRepo(input)
			return jsonFrame(status, "", value)
		case "GET":
			f.mu.RLock()
			names := make([]string, 0, len(f.repos))
			for name := range f.repos {
				names = append(names, name)
			}
			sort.Strings(names)
			data := make([]obj, 0, len(names))
			for _, name := range names {
				r := f.repos[name]
				r.mu.RLock()
				data = append(data, r.view())
				r.mu.RUnlock()
			}
			f.mu.RUnlock()
			values, _ := url.ParseQuery(q.query)
			return paginate(q, values, data)
		}
	}
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 4)
	if len(parts) < 3 || parts[0] != "repos" {
		return notFound
	}
	r := f.repo(parts[1], parts[2])
	if r == nil {
		return notFound
	}
	action := ""
	if len(parts) == 4 {
		action = parts[3]
	}
	if q.method == "GET" && strings.HasPrefix(action, "issues/") && !strings.Contains(action[7:], "/") {
		n, err := strconv.Atoi(action[7:])
		if err == nil {
			if cached, found := r.readIndex.Load(n); found {
				return cached.(response)
			}
		}
		return notFound
	}
	if q.method == "GET" && (action == "issues" || action == "pulls") {
		values, _ := url.ParseQuery(q.query)
		cacheKey := action + ":" + q.host + "?" + q.query
		if cached := r.cachedFrame(cacheKey); cached != nil {
			return cached
		}
		r.mu.RLock()
		defer r.mu.RUnlock()
		if cached := r.cachedFrame(cacheKey); cached != nil {
			return cached
		}
		var result response
		if action == "issues" {
			result = paginate(q, values, r.issueList(values))
		} else {
			state := values.Get("state")
			if state == "" {
				state = "open"
			}
			list := make([]obj, 0, len(r.pulls))
			for _, p := range r.pulls {
				if state == "all" || state == p.state {
					list = append(list, r.pullView(p))
				}
			}
			sort.Slice(list, func(i, j int) bool { return integer(list[i]["number"]) > integer(list[j]["number"]) })
			result = paginate(q, values, list)
		}
		r.storeFrame(cacheKey, result)
		return result
	}
	if action == "" && q.method == "DELETE" {
		f.mu.Lock()
		delete(f.repos, strings.ToLower(parts[2]))
		f.mu.Unlock()
		return noContent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if action == "" {
		switch q.method {
		case "GET":
			return jsonFrame(200, "", r.view())
		case "PATCH":
			if description, ok := input["description"].(string); ok {
				r.description = description
			}
			if branch, ok := input["default_branch"].(string); ok && r.refs["refs/heads/"+branch] != zero {
				r.branch = branch
			}
			r.invalidateFrames()
			return jsonFrame(200, "", r.view())
		}
		return notFound
	}
	values, _ := url.ParseQuery(q.query)
	if action == "git/refs" && q.method == "POST" {
		name, id := text(input["ref"]), idFromString(text(input["sha"]))
		if !validRef(name) || r.objects[id] == nil || r.refs[name] != zero {
			return problem(422, "Invalid reference")
		}
		r.refs[name] = id
		r.invalidateFrames()
		return jsonFrame(201, "", obj{"ref": name, "object": obj{"sha": id.String(), "type": "commit"}})
	}
	if strings.HasPrefix(action, "git/ref/") && q.method == "GET" {
		name := "refs/" + action[len("git/ref/"):]
		if id := r.refs[name]; id != zero {
			return jsonFrame(200, "", obj{"ref": name, "object": obj{"sha": id.String()}})
		}
		return notFound
	}
	if action == "branches" && q.method == "GET" {
		data := make([]obj, 0)
		for _, name := range r.refNames() {
			if strings.HasPrefix(name, "refs/heads/") {
				data = append(data, obj{"name": name[len("refs/heads/"):], "commit": obj{"sha": r.refs[name].String()}, "protected": false})
			}
		}
		return paginate(q, values, data)
	}
	if action == "commits" && q.method == "GET" {
		id := r.resolve(values.Get("sha"))
		data := make([]obj, 0)
		queue := []oid{id}
		seen := make(map[oid]bool)
		for i := 0; i < len(queue); i++ {
			g := r.objects[queue[i]]
			if g == nil || g.kind != commitKind || seen[g.id] {
				continue
			}
			seen[g.id] = true
			data = append(data, commitView(g))
			queue = append(queue, g.parents...)
		}
		return paginate(q, values, data)
	}
	if strings.HasPrefix(action, "commits/") && q.method == "GET" {
		if g := r.objects[r.resolve(action[8:])]; g != nil && g.kind == commitKind {
			return jsonFrame(200, "", commitView(g))
		}
		return notFound
	}
	if action == "contents" || strings.HasPrefix(action, "contents/") {
		return r.contents(q, input, values, strings.TrimPrefix(strings.TrimPrefix(action, "contents"), "/"))
	}
	if action == "labels" {
		if q.method == "POST" {
			name := text(input["name"])
			if name == "" {
				return problem(422, "Label name is required")
			}
			label := obj{"name": name, "color": text(input["color"])}
			r.labels[name] = label
			return jsonFrame(201, "", label)
		}
		if q.method == "GET" {
			list := make([]obj, 0, len(r.labels))
			for _, label := range r.labels {
				list = append(list, label)
			}
			sort.Slice(list, func(i, j int) bool { return text(list[i]["name"]) < text(list[j]["name"]) })
			return jsonFrame(200, "", list)
		}
	}
	if action == "issues" {
		if q.method == "POST" {
			status, value := r.createIssue(input)
			if status == 201 {
				// refreshIssue already encoded this object for subsequent GETs.
				_, body, _ := bytes.Cut(r.issues[r.nextNumber].read, []byte("\r\n\r\n"))
				return frame(status, "application/json", "", body)
			}
			return jsonFrame(status, "", value)
		}
	}
	if strings.HasPrefix(action, "issues/") {
		num, tail, _ := strings.Cut(action[7:], "/")
		i := r.issues[integer(num)]
		if i == nil {
			return notFound
		}
		if tail == "comments" {
			if q.method == "GET" {
				return jsonFrame(200, "", i.comments)
			}
			if q.method == "POST" {
				if text(input["body"]) == "" {
					return problem(422, "Comment body is required")
				}
				comment := r.addComment(i, text(input["body"]))
				return jsonFrame(201, "", comment)
			}
		}
		if tail == "" && q.method == "PATCH" {
			if state, ok := input["state"].(string); ok && (state == "open" || state == "closed") {
				i.state = state
			}
			if title, ok := input["title"].(string); ok {
				i.title = title
			}
			if body, ok := input["body"].(string); ok {
				i.body = body
			}
			r.refreshIssue(i)
			return i.read
		}
	}
	if action == "pulls" {
		if q.method == "POST" {
			status, value := r.createPull(input)
			return jsonFrame(status, "", value)
		}
	}
	if strings.HasPrefix(action, "pulls/") {
		num, tail, _ := strings.Cut(action[6:], "/")
		p := r.pulls[integer(num)]
		if p == nil {
			return notFound
		}
		if tail == "" && q.method == "GET" {
			return jsonFrame(200, "", r.pullView(p))
		}
		if tail == "files" && q.method == "GET" {
			return paginate(q, values, r.pullFiles(p))
		}
		if tail == "merge" && q.method == "PUT" {
			status, value := r.mergePull(p)
			return jsonFrame(status, "", value)
		}
	}
	return notFound
}

func commitView(g *gitObject) obj {
	_, message, _ := bytes.Cut(g.data, []byte("\n\n"))
	parents := make([]obj, 0, len(g.parents))
	for _, parent := range g.parents {
		parents = append(parents, obj{"sha": parent.String()})
	}
	return obj{"sha": g.id.String(), "commit": obj{"message": strings.TrimSuffix(string(message), "\n"), "tree": obj{"sha": g.target.String()}}, "parents": parents}
}

func (r *repository) contentView(name string, entry treeEntry) obj {
	g := r.objects[entry.id]
	if g == nil {
		return obj{}
	}
	return obj{"name": path.Base(name), "path": name, "sha": entry.id.String(), "size": len(g.data), "type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(g.data)}
}

func safePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for segment := range strings.SplitSeq(p, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.EqualFold(segment, ".git") {
			return false
		}
	}
	return true
}

func (r *repository) contents(q request, input obj, values url.Values, name string) response {
	branch := text(input["branch"])
	if branch == "" {
		branch = r.branch
	}
	ref := branch
	if q.method == "GET" {
		ref = values.Get("ref")
	}
	parent := r.resolve(ref)
	files := r.filesAt(parent)
	entry, exists := files[name]
	if q.method == "GET" {
		if exists {
			return jsonFrame(200, "", r.contentView(name, entry))
		}
		prefix := strings.TrimSuffix(name, "/")
		if prefix != "" {
			prefix += "/"
		}
		list := make([]obj, 0)
		dirs := make(map[string]bool)
		for file, entry := range files {
			if !strings.HasPrefix(file, prefix) {
				continue
			}
			tail := strings.TrimPrefix(file, prefix)
			if head, _, dir := strings.Cut(tail, "/"); dir {
				if !dirs[head] {
					list = append(list, obj{"name": head, "path": prefix + head, "type": "dir", "sha": zero.String(), "size": 0})
					dirs[head] = true
				}
			} else {
				list = append(list, r.contentView(file, entry))
			}
		}
		if len(list) == 0 && name != "" {
			return notFound
		}
		sort.Slice(list, func(i, j int) bool { return text(list[i]["name"]) < text(list[j]["name"]) })
		return jsonFrame(200, "", list)
	}
	if q.method != "PUT" && q.method != "DELETE" {
		return notFound
	}
	if !safePath(name) || !validRef("refs/heads/"+branch) {
		return problem(422, "Invalid file path or branch")
	}
	if parent == zero && branch != r.branch {
		return notFound
	}
	sha := text(input["sha"])
	if exists && sha != entry.id.String() || !exists && sha != "" {
		return problem(409, "Content sha does not match")
	}
	if q.method == "DELETE" {
		if !exists {
			return notFound
		}
		delete(files, name)
	} else {
		value, ok := input["content"].(string)
		if !ok {
			return problem(422, "Content is required")
		}
		data, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return problem(422, "Invalid base64 content")
		}
		entry = treeEntry{path.Base(name), "100644", r.saveObject(blobKind, data)}
		files[name] = entry
	}
	id := r.commitFiles(files, parent, text(input["message"]), r.owner)
	r.refs["refs/heads/"+branch] = id
	r.invalidateFrames()
	status := 200
	var content any
	if q.method != "DELETE" {
		content = r.contentView(name, entry)
		if !exists {
			status = 201
		}
	}
	return jsonFrame(status, "", obj{"commit": obj{"sha": id.String()}, "content": content})
}

func (r *repository) issueBase(i *issue) obj {
	return obj{"id": i.id, "node_id": i.nodeID, "number": i.number, "title": i.title, "body": i.body, "state": i.state, "labels": i.labels, "comments": len(i.comments), "user": obj{"login": r.owner}, "created_at": "2000-01-01T00:00:00Z", "updated_at": "2000-01-01T00:00:00Z"}
}

func (r *repository) refreshIssue(i *issue) {
	i.view = r.issueBase(i)
	i.read = jsonFrame(200, "", i.view)
	r.readIndex.Store(i.number, i.read)
	r.invalidateFrames()
}

func (r *repository) createIssue(input obj) (int, any) {
	title := text(input["title"])
	if title == "" {
		return 422, obj{"message": "Title is required"}
	}
	labels := make([]obj, 0)
	for _, value := range items(input["labels"]) {
		name := text(value)
		if name == "" {
			name = text(object(value)["name"])
		}
		label := r.labels[name]
		if label == nil {
			return 422, obj{"message": "Unknown label"}
		}
		labels = append(labels, label)
	}
	r.nextNumber++
	n := r.nextNumber
	i := &issue{id: strconv.FormatInt(r.id, 10) + "-" + strconv.Itoa(n), nodeID: globalID("Issue", r.id, n), number: n, title: title, body: text(input["body"]), state: "open", labels: labels, comments: []obj{}}
	r.issues[n] = i
	r.refreshIssue(i)
	return 201, i.view
}

func (r *repository) issueList(values url.Values) []obj {
	state := values.Get("state")
	if state == "" {
		state = "open"
	}
	list := make([]obj, 0, len(r.issues))
	for _, i := range r.issues {
		if state == "all" || i.state == state {
			list = append(list, i.view)
		}
	}
	asc := values.Get("direction") == "asc"
	sort.Slice(list, func(i, j int) bool {
		if asc {
			return integer(list[i]["number"]) < integer(list[j]["number"])
		}
		return integer(list[i]["number"]) > integer(list[j]["number"])
	})
	return list
}

func (r *repository) addComment(i *issue, body string) obj {
	r.nextComment++
	comment := obj{"id": r.nextComment, "node_id": globalID("IssueComment", r.id, r.nextComment), "body": body, "user": obj{"login": r.owner}}
	i.comments = append(i.comments, comment)
	r.refreshIssue(i)
	return comment
}

func (r *repository) createPull(input obj) (int, any) {
	head, base := text(input["head"]), text(input["base"])
	if owner, name, ok := strings.Cut(head, ":"); ok {
		if owner != r.owner {
			return 422, obj{"message": "Unknown head owner"}
		}
		head = name
	}
	headID, baseID := r.resolve(head), r.resolve(base)
	if text(input["title"]) == "" || head == "" || base == "" || headID == zero || baseID == zero {
		return 422, obj{"message": "Invalid pull request"}
	}
	r.nextNumber++
	n := r.nextNumber
	p := &pull{issue: issue{id: strconv.FormatInt(r.id, 10) + "-" + strconv.Itoa(n), nodeID: globalID("PullRequest", r.id, n), number: n, title: text(input["title"]), body: text(input["body"]), state: "open", labels: []obj{}, comments: []obj{}}, head: head, base: base, headID: headID, baseID: baseID, forkID: r.mergeBase(baseID, headID)}
	r.pulls[n] = p
	r.invalidateFrames()
	return 201, r.pullView(p)
}

func equalEntry(a, b treeEntry) bool { return a.id == b.id && a.mode == b.mode }

func (r *repository) mergedFiles(p *pull) (map[string]treeEntry, bool) {
	base := r.filesAt(r.resolve(p.base))
	head := r.filesAt(r.resolve(p.head))
	original := r.filesAt(p.forkID)
	paths := make(map[string]bool, len(head)+len(original))
	for name := range head {
		paths[name] = true
	}
	for name := range original {
		paths[name] = true
	}
	for name := range paths {
		h, b, o := head[name], base[name], original[name]
		if equalEntry(h, o) {
			continue
		}
		if !equalEntry(b, o) && !equalEntry(b, h) {
			return nil, false
		}
		if h.id == zero {
			delete(base, name)
		} else {
			base[name] = h
		}
	}
	return base, true
}

func (r *repository) pullView(p *pull) obj {
	value := r.issueBase(&p.issue)
	headID, baseID := r.resolve(p.head), r.resolve(p.base)
	if headID == zero {
		headID = p.headID
	}
	if baseID == zero {
		baseID = p.baseID
	}
	value["head"] = obj{"ref": p.head, "sha": headID.String()}
	value["base"] = obj{"ref": p.base, "sha": baseID.String()}
	value["merged"] = p.merged
	_, value["mergeable"] = r.mergedFiles(p)
	value["merged_at"], value["merge_commit_sha"] = nil, nil
	if p.merged {
		value["merged_at"] = "2000-01-01T00:00:00Z"
		value["merge_commit_sha"] = p.mergeID.String()
	}
	return value
}

func lineCount(g *gitObject) int {
	if g == nil || len(g.data) == 0 {
		return 0
	}
	n := bytes.Count(g.data, []byte{'\n'})
	if g.data[len(g.data)-1] != '\n' {
		n++
	}
	return n
}

func (r *repository) pullFiles(p *pull) []obj {
	original := r.filesAt(p.forkID)
	head := r.filesAt(r.resolve(p.head))
	paths := make(map[string]bool, len(original)+len(head))
	for name := range original {
		paths[name] = true
	}
	for name := range head {
		paths[name] = true
	}
	names := make([]string, 0, len(paths))
	for name := range paths {
		if !equalEntry(original[name], head[name]) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	list := make([]obj, 0, len(names))
	for _, name := range names {
		before, after := original[name], head[name]
		status := "modified"
		if before.id == zero {
			status = "added"
		} else if after.id == zero {
			status = "removed"
		}
		added, deleted := lineCount(r.objects[after.id]), lineCount(r.objects[before.id])
		list = append(list, obj{"filename": name, "path": name, "status": status, "additions": added, "deletions": deleted, "changes": added + deleted})
	}
	return list
}

func (r *repository) mergePull(p *pull) (int, any) {
	if p.merged {
		return 200, obj{"sha": p.mergeID.String(), "merged": true, "message": "Already merged"}
	}
	files, ok := r.mergedFiles(p)
	if !ok || p.state != "open" {
		return 405, obj{"message": "Pull request cannot be merged"}
	}
	parent := r.resolve(p.base)
	id := r.commitFiles(files, parent, p.title+"\n\n"+p.body, r.owner)
	r.refs["refs/heads/"+p.base] = id
	p.mergeID, p.merged, p.state = id, true, "closed"
	r.invalidateFrames()
	return 200, obj{"sha": id.String(), "merged": true, "message": "Pull request successfully merged"}
}

// The diagnostic export serializes real forge state in stable order. Connection
// buffers, port numbers, response caches and execution times never enter it.
func (f *forge) exportState() response {
	f.mu.RLock()
	defer f.mu.RUnlock()
	names := make([]string, 0, len(f.repos))
	for name := range f.repos {
		names = append(names, name)
	}
	sort.Strings(names)
	repos := make([]obj, 0, len(names))
	for _, name := range names {
		r := f.repos[name]
		r.mu.RLock()
		value := r.view()
		refs := make(map[string]string, len(r.refs))
		for name, id := range r.refs {
			refs[name] = id.String()
		}
		value["refs"] = refs
		objects := make(map[string]any, len(r.objects))
		for id, g := range r.objects {
			objects[id.String()] = obj{"type": kindName[g.kind], "data": base64.StdEncoding.EncodeToString(g.data)}
		}
		value["objects"] = objects
		issues := make([]obj, 0, len(r.issues))
		pulls := make([]obj, 0, len(r.pulls))
		for n := 1; n <= r.nextNumber; n++ {
			if i := r.issues[n]; i != nil {
				v := r.issueBase(i)
				v["comments"] = i.comments
				issues = append(issues, v)
			}
			if p := r.pulls[n]; p != nil {
				pulls = append(pulls, r.pullView(p))
			}
		}
		value["issues"], value["pulls"], value["labels"] = issues, pulls, r.labels
		repos = append(repos, value)
		r.mu.RUnlock()
	}
	return jsonFrame(200, "", obj{"version": 1, "repositories": repos})
}

// Used by GraphQL's node lookup; ids encode only committed store identities.
func (f *forge) findNode(id string) (*repository, string, int) {
	decoded, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil, "", 0
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) != 3 {
		return nil, "", 0
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, "", 0
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, r := range f.repos {
		if r.id == n {
			return r, parts[0], integer(parts[2])
		}
	}
	return nil, "", 0
}

func copyMap(in obj) obj {
	out := make(obj, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

var _ = json.Valid
