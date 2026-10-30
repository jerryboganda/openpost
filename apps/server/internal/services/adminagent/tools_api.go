package adminagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"unicode"
)

const (
	apiBasePath         = "/api/v1"
	maxAPISearchResults = 10
	maxAPIResponseBytes = 256 << 10
	maxSpecBytes        = 16 << 20
	maxOutlineDepth     = 3
	maxOutlineChars     = 7000
)

var httpMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}

type apiOperation struct {
	method   string
	path     string
	id       string
	summary  string
	haystack string
	raw      map[string]any
}

// apiSpec is a searchable index of the running server's OpenAPI description.
type apiSpec struct {
	operations []apiOperation
	schemas    map[string]any
}

type specCache struct {
	mu   sync.Mutex
	spec *apiSpec
}

// captureWriter records a handler's response in memory, bounded by limit.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	limit  int
	over   int
}

func newCaptureWriter(limit int) *captureWriter {
	return &captureWriter{header: make(http.Header), limit: limit}
}

func (w *captureWriter) Header() http.Header { return w.header }

func (w *captureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *captureWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	room := max(0, w.limit-w.body.Len())
	kept := min(room, len(data))
	w.body.Write(data[:kept])
	w.over += len(data) - kept
	return len(data), nil
}

// Flush satisfies handlers that stream; the capture is read after the handler returns.
func (w *captureWriter) Flush() {}

type internalReply struct {
	status    int
	body      []byte
	truncated bool
}

// serveInternal sends a request through the application's own router. The
// request is authenticated like any browser request, so the same middleware,
// permissions, and validation apply.
func (s *Service) serveInternal(ctx context.Context, method, target, token string, payload []byte, limit int) (internalReply, error) {
	handler := s.internalAPI
	if handler == nil {
		return internalReply{}, errToolNotReady
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return internalReply{}, fmt.Errorf("the request could not be built: %w", err)
	}
	request.RemoteAddr = "127.0.0.1:0"
	if parsed, parseErr := url.Parse(s.env.PublicURL); parseErr == nil && parsed.Host != "" {
		request.Host = parsed.Host
		if parsed.Scheme == "https" {
			request.Header.Set("X-Forwarded-Proto", "https")
		}
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	writer := newCaptureWriter(limit)
	handler.ServeHTTP(writer, request)
	status := writer.status
	if status == 0 {
		status = http.StatusOK
	}
	return internalReply{status: status, body: writer.body.Bytes(), truncated: writer.over > 0}, nil
}

func (s *Service) loadAPISpec(ctx context.Context) (*apiSpec, error) {
	s.specs.mu.Lock()
	defer s.specs.mu.Unlock()
	if s.specs.spec != nil {
		return s.specs.spec, nil
	}
	reply, err := s.serveInternal(ctx, http.MethodGet, "/openapi.json", "", nil, maxSpecBytes)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK || reply.truncated {
		return nil, fmt.Errorf("the API description is not available (HTTP %d)", reply.status)
	}
	var doc map[string]any
	if err := json.Unmarshal(reply.body, &doc); err != nil {
		return nil, errors.New("the API description could not be read")
	}
	s.specs.spec = indexAPISpec(doc)
	return s.specs.spec, nil
}

func indexAPISpec(doc map[string]any) *apiSpec {
	spec := &apiSpec{schemas: map[string]any{}}
	if components, ok := doc["components"].(map[string]any); ok {
		if schemas, ok := components["schemas"].(map[string]any); ok {
			spec.schemas = schemas
		}
	}
	paths, _ := doc["paths"].(map[string]any)
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, value := range methods {
			operation, _ := value.(map[string]any)
			if operation == nil || !httpMethods[strings.ToLower(method)] {
				continue
			}
			id, _ := operation["operationId"].(string)
			summary, _ := operation["summary"].(string)
			tags := ""
			if list, ok := operation["tags"].([]any); ok {
				for _, tag := range list {
					tags += " " + fmt.Sprint(tag)
				}
			}
			spec.operations = append(spec.operations, apiOperation{
				method: strings.ToUpper(method), path: path, id: id, summary: summary, raw: operation,
				haystack: strings.ToLower(method + " " + path + " " + id + " " + summary + tags),
			})
		}
	}
	sort.Slice(spec.operations, func(i, j int) bool {
		if spec.operations[i].path != spec.operations[j].path {
			return spec.operations[i].path < spec.operations[j].path
		}
		return spec.operations[i].method < spec.operations[j].method
	})
	return spec
}

func searchTokens(query string) []string {
	return strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func (sp *apiSpec) search(query string) []apiOperation {
	tokens := searchTokens(query)
	if len(tokens) == 0 {
		return nil
	}
	type scored struct {
		operation apiOperation
		matched   int
		score     int
	}
	var hits []scored
	for _, operation := range sp.operations {
		hit := scored{operation: operation}
		for _, token := range tokens {
			if !strings.Contains(operation.haystack, token) {
				continue
			}
			hit.matched++
			hit.score++
			if strings.Contains(strings.ToLower(operation.id), token) {
				hit.score += 2
			}
			if strings.Contains(strings.ToLower(operation.path), token) {
				hit.score++
			}
		}
		if hit.matched*2 >= len(tokens) {
			hits = append(hits, hit)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].matched != hits[j].matched {
			return hits[i].matched > hits[j].matched
		}
		return hits[i].score > hits[j].score
	})
	result := make([]apiOperation, 0, maxAPISearchResults)
	for _, hit := range hits {
		if len(result) == maxAPISearchResults {
			break
		}
		result = append(result, hit.operation)
	}
	return result
}

func (sp *apiSpec) byID(id string) (apiOperation, bool) {
	for _, operation := range sp.operations {
		if strings.EqualFold(operation.id, id) {
			return operation, true
		}
	}
	return apiOperation{}, false
}

func (operation apiOperation) line() string {
	return fmt.Sprintf("%s %s%s | %s | %s", operation.method, apiBasePath, operation.path, operation.id, operation.summary)
}

// typeText renders a schema as compact pseudo-JSON that is cheap for a model to
// read: optional fields end in ?, and nesting stops at a fixed depth.
func (sp *apiSpec) typeText(schema map[string]any, depth int, seen map[string]bool, indent string) string {
	if ref, ok := schema["$ref"].(string); ok {
		return sp.refText(ref, depth, seen, indent)
	}
	if names := sp.unionNames(schema, seen, indent); names != nil {
		return strings.Join(names, " | ")
	}
	switch schema["type"] {
	case "array":
		items, _ := schema["items"].(map[string]any)
		return "[" + sp.typeText(items, depth, seen, indent) + "]"
	case "object":
		return sp.objectText(schema, depth, seen, indent)
	}
	return scalarText(schema)
}

// refText expands a referenced schema once. A cycle, an unknown name, or the
// depth limit leaves the bare schema name.
func (sp *apiSpec) refText(ref string, depth int, seen map[string]bool, indent string) string {
	name := ref[strings.LastIndex(ref, "/")+1:]
	resolved, _ := sp.schemas[name].(map[string]any)
	if resolved == nil || seen[name] || depth <= 0 {
		return name
	}
	seen[name] = true
	defer delete(seen, name)
	return sp.typeText(resolved, depth, seen, indent)
}

// unionNames lists the alternatives of a oneOf or anyOf schema by name, or
// returns nil when the schema is not a union.
func (sp *apiSpec) unionNames(schema map[string]any, seen map[string]bool, indent string) []string {
	for _, key := range []string{"oneOf", "anyOf"} {
		options, ok := schema[key].([]any)
		if !ok {
			continue
		}
		names := make([]string, 0, len(options))
		for _, option := range options {
			if mapped, isMap := option.(map[string]any); isMap {
				names = append(names, sp.typeText(mapped, 0, seen, indent))
			}
		}
		return names
	}
	return nil
}

func scalarText(schema map[string]any) string {
	text, _ := schema["type"].(string)
	if text == "" {
		text = "any"
	}
	if format, ok := schema["format"].(string); ok && format != "" {
		text += "(" + format + ")"
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 && len(values) <= 10 {
		options := make([]string, 0, len(values))
		for _, value := range values {
			options = append(options, fmt.Sprint(value))
		}
		text += " " + strings.Join(options, "|")
	}
	return text
}

func (sp *apiSpec) objectText(schema map[string]any, depth int, seen map[string]bool, indent string) string {
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) == 0 || depth <= 0 {
		return "object"
	}
	required := make(map[string]bool)
	if list, ok := schema["required"].([]any); ok {
		for _, name := range list {
			required[fmt.Sprint(name)] = true
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString("{\n")
	for _, name := range names {
		property, _ := properties[name].(map[string]any)
		if property == nil || name == "$schema" {
			continue
		}
		mark := "?"
		if required[name] {
			mark = ""
		}
		builder.WriteString(indent + "  " + name + mark + ": " + sp.typeText(property, depth-1, seen, indent+"  "))
		if description, ok := property["description"].(string); ok && description != "" {
			builder.WriteString("  // " + firstLine(description, 80))
		}
		builder.WriteString("\n")
	}
	builder.WriteString(indent + "}")
	return builder.String()
}

func (sp *apiSpec) describe(operation apiOperation) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s %s%s (operationId %s)\n", operation.method, apiBasePath, operation.path, operation.id)
	if operation.summary != "" {
		builder.WriteString(operation.summary + "\n")
	}
	if description, ok := operation.raw["description"].(string); ok && description != "" {
		builder.WriteString(clipText(strings.Join(strings.Fields(description), " "), 400) + "\n")
	}
	if parameters, ok := operation.raw["parameters"].([]any); ok && len(parameters) > 0 {
		builder.WriteString("Parameters:\n")
		for _, entry := range parameters {
			parameter, _ := entry.(map[string]any)
			name, _ := parameter["name"].(string)
			location, _ := parameter["in"].(string)
			schema, _ := parameter["schema"].(map[string]any)
			required := ""
			if flag, _ := parameter["required"].(bool); flag {
				required = " (required)"
			}
			fmt.Fprintf(&builder, "  %s %s: %s%s\n", location, name, sp.typeText(schema, 1, map[string]bool{}, "  "), required)
		}
	}
	if body, ok := operation.raw["requestBody"].(map[string]any); ok {
		content, _ := body["content"].(map[string]any)
		for mediaType, value := range content {
			entry, _ := value.(map[string]any)
			schema, _ := entry["schema"].(map[string]any)
			fmt.Fprintf(&builder, "Request body (%s):\n%s\n", mediaType, sp.typeText(schema, maxOutlineDepth, map[string]bool{}, ""))
			break
		}
	}
	return clipText(builder.String(), maxOutlineChars)
}

func (s *Service) apiSearchTool() *toolSpec {
	return &toolSpec{
		name:        "api_search",
		description: "Find OpenPost API operations. Search with words such as \"schedule publication\" or \"list accounts\". Pass operation_id to see one operation's parameters and request body before calling it. The API that powers the web app, CLI, and MCP is the same one, so every product action is available.",
		parameters: objectSchema(nil, map[string]any{
			"query":        stringSchema("Words that describe the operation."),
			"operation_id": stringSchema("Exact operation id from an earlier search, to see its parameters and request body."),
		}),
		describe: func(a toolArgs) string {
			if id := a.trimmed("operation_id"); id != "" {
				return "Look up API operation " + id
			}
			return "Search the API for " + a.trimmed("query")
		},
		run: func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) {
			spec, err := s.loadAPISpec(ctx)
			if err != nil {
				return "", err
			}
			if id := a.trimmed("operation_id"); id != "" {
				operation, found := spec.byID(id)
				if !found {
					return "", fmt.Errorf("no operation has the id %q; search by words first", id)
				}
				return spec.describe(operation), nil
			}
			query := a.trimmed("query")
			if query == "" {
				return "", errors.New("query or operation_id is required")
			}
			matches := spec.search(query)
			if len(matches) == 0 {
				return fmt.Sprintf("No operation matches %q. Try fewer or different words.", query), nil
			}
			lines := make([]string, 0, len(matches))
			for _, operation := range matches {
				lines = append(lines, operation.line())
			}
			return "method path | operationId | summary\n" + strings.Join(lines, "\n") + "\nUse operation_id to see the parameters and body of one operation.", nil
		},
	}
}

func (s *Service) apiCallTool() *toolSpec {
	return &toolSpec{
		name:        "api_call",
		description: "Call the OpenPost API as the administrator. Paths start with /api/v1 or are relative to it. The request obeys the same permissions and validation as the web app. Publishing, scheduling, deleting, and billing actions are real: make them only when the administrator asked for them, and read the operation with api_search first.",
		parameters: objectSchema([]string{"path"}, map[string]any{
			"method": enumSchema("HTTP method. Defaults to GET.", "GET", "POST", "PUT", "PATCH", "DELETE"),
			"path":   stringSchema("API path such as /workspaces or /api/v1/workspaces."),
			"query":  objectProperty("Query string parameters."),
			"body":   objectProperty("JSON request body."),
		}),
		describe: func(a toolArgs) string {
			method := strings.ToUpper(a.trimmed("method"))
			if method == "" {
				method = http.MethodGet
			}
			return "OpenPost API " + method + " " + a.trimmed("path")
		},
		run: func(ctx context.Context, rt *toolRuntime, a toolArgs) (string, error) { return s.callAPI(ctx, rt, a) },
	}
}

func apiTarget(raw, method string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.TrimSpace(raw) == "" {
		return "", errors.New("path is required and must be a valid path")
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		return "", errors.New("use a path such as /workspaces, not a full URL")
	}
	path := parsed.Path
	if path != apiBasePath && !strings.HasPrefix(path, apiBasePath+"/") {
		path = apiBasePath + "/" + strings.TrimLeft(path, "/")
	}
	if err := checkAPIPath(path, method); err != nil {
		return "", err
	}
	// Re-encoding keeps the request path identical to the path that was checked.
	target := url.URL{Path: path, RawQuery: parsed.RawQuery}
	return target.String(), nil
}

// checkAPIPath keeps the agent away from its own control endpoints and from
// account security and backup changes, which belong to the administrator.
func checkAPIPath(path, method string) error {
	lower := strings.ToLower(path)
	switch {
	case strings.Contains(path, "..") || strings.Contains(path, "//"):
		return errors.New("path is not valid")
	case lower == apiBasePath+"/admin/agent" || strings.HasPrefix(lower, apiBasePath+"/admin/agent/"):
		return errors.New("the agent cannot call its own control endpoints")
	case method != http.MethodGet && strings.HasPrefix(lower, apiBasePath+"/auth/"):
		return errors.New("sign-in, password, and security settings cannot be changed by the agent")
	case method != http.MethodGet && strings.Contains(lower, "backup"):
		return errors.New("backups and restores are run from the instance settings page")
	}
	return nil
}

func (s *Service) callAPI(ctx context.Context, rt *toolRuntime, args toolArgs) (string, error) {
	method := strings.ToUpper(args.trimmed("method"))
	switch method {
	case "":
		method = http.MethodGet
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return "", errors.New("method must be GET, POST, PUT, PATCH, or DELETE")
	}
	if method != http.MethodGet && rt.access == AccessReadOnly {
		return "", errReadOnlyAccess
	}
	target, err := apiTarget(args.text("path"), method)
	if err != nil {
		return "", err
	}
	if query := args.object("query"); len(query) > 0 {
		values := url.Values{}
		for key, value := range query {
			values.Set(key, scalarString(value))
		}
		separator := "?"
		if strings.Contains(target, "?") {
			separator = "&"
		}
		target += separator + values.Encode()
	}
	var payload []byte
	if body := args.object("body"); body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return "", errors.New("body could not be encoded as JSON")
		}
		payload = encoded
	}
	token, err := rt.bearer(ctx)
	if err != nil {
		return "", err
	}
	reply, err := s.serveInternal(ctx, method, target, token, payload, maxAPIResponseBytes)
	if err != nil {
		return "", err
	}
	text := string(reply.body)
	if reply.truncated {
		text += "\n…the response was larger than the capture limit and was cut off"
	}
	return fmt.Sprintf("HTTP %d\n%s", reply.status, clipText(text, maxToolOutput)), nil
}
