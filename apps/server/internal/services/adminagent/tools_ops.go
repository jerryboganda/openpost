package adminagent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultLogLines   = 60
	maxLogLines       = 300
	maxLogLineWidth   = 500
	defaultQueryRows  = 50
	maxQueryRows      = 200
	maxQueryCellWidth = 200
	queryTimeoutMs    = 15000
)

var (
	sensitiveColumnPattern = regexp.MustCompile(`(?i)(password|passwd|secret|token|hash|encrypted|credential|api_key|private|salt|totp|recovery|ciphertext|signature)`)
	forbiddenSQLPattern    = regexp.MustCompile(`(?i)\b(insert|update|delete|drop|alter|truncate|create|grant|revoke|copy|vacuum|attach|detach|pragma|reindex|analyze|into|execute|call|merge)\b`)
	logLevelPattern        = regexp.MustCompile(`"severity":"(WARN|ERROR)"`)
	serverStatusPattern    = regexp.MustCompile(`"status":5\d\d`)
	legacyProblemPattern   = regexp.MustCompile(`(?i)(error|fail|panic|fatal|timeout|denied)`)
)

// SetEnvironment tells the agent which instance it is running in.
func (s *Service) SetEnvironment(environment Environment) {
	s.env = environment
}

// SetLogSource lets the agent read recent log lines.
func (s *Service) SetLogSource(source LogSource) {
	s.logs = source
}

// SetSessionIssuer lets the agent call the OpenPost API as the administrator.
func (s *Service) SetSessionIssuer(issuer SessionIssuer) {
	s.sessions = issuer
}

func (s *Service) operationsTools() []*toolSpec {
	return []*toolSpec{
		{
			name:        "prod_status",
			description: "Show the running production instance: version and revision, uptime, database readiness, and record counts. Use it first when something may be wrong in production.",
			parameters:  objectSchema(nil, map[string]any{}),
			describe:    func(toolArgs) string { return "Check production status" },
			run: func(ctx context.Context, _ *toolRuntime, _ toolArgs) (string, error) {
				return s.productionStatus(ctx), nil
			},
		},
		{
			name:        "prod_logs",
			description: "Read recent structured log lines from the running server, newest last. By default it shows warnings, errors, and server errors only. Set kind to all for everything, and contains to filter by text such as a path or error message.",
			parameters: objectSchema(nil, map[string]any{
				"kind":     enumSchema("problems (default) or all.", "problems", "all"),
				"contains": stringSchema("Only lines that contain this text."),
				"limit":    integerSchema("Lines to return, 1 to 300. Defaults to 60."),
			}),
			describe: func(a toolArgs) string {
				if filter := a.trimmed("contains"); filter != "" {
					return "Read server logs containing " + filter
				}
				return "Read recent server logs"
			},
			run: func(_ context.Context, _ *toolRuntime, a toolArgs) (string, error) { return s.readLogs(a) },
		},
		{
			name:        "db_query",
			description: "Run one read-only SQL SELECT against the production database. Use information_schema.tables and information_schema.columns on PostgreSQL to discover the schema. Columns that hold secrets are masked. Changes to data go through api_call, never SQL.",
			parameters: objectSchema([]string{"sql"}, map[string]any{
				"sql":   stringSchema("A single SELECT or WITH ... SELECT statement without a trailing semicolon."),
				"limit": integerSchema("Rows to return, 1 to 200. Defaults to 50."),
			}),
			describe: func(a toolArgs) string { return "Query: " + firstLine(a.trimmed("sql"), 100) },
			run:      func(ctx context.Context, _ *toolRuntime, a toolArgs) (string, error) { return s.readOnlyQuery(ctx, a) },
		},
		s.apiSearchTool(),
		s.apiCallTool(),
	}
}

func (s *Service) productionStatus(ctx context.Context) string {
	var builder strings.Builder
	builder.WriteString("OpenPost production status\n")
	fmt.Fprintf(&builder, "Public URL: %s\n", orUnknown(s.env.PublicURL))
	fmt.Fprintf(&builder, "Version: %s, revision %s, edition %s\n", orUnknown(s.env.Version), orUnknown(s.env.Revision), orUnknown(s.env.Edition))
	if !s.env.StartedAt.IsZero() {
		fmt.Fprintf(&builder, "Started: %s (up %s)\n", s.env.StartedAt.UTC().Format(time.RFC3339), time.Since(s.env.StartedAt).Round(time.Second))
	}
	started := time.Now()
	if err := s.db.PingContext(ctx); err != nil {
		fmt.Fprintf(&builder, "Database (%s): NOT READY (%v)\n", orUnknown(s.env.DatabaseDriver), err)
	} else {
		fmt.Fprintf(&builder, "Database (%s): ready, ping %d ms\n", orUnknown(s.env.DatabaseDriver), time.Since(started).Milliseconds())
	}
	fmt.Fprintf(&builder, "AI provider: %s, default model %s\n", orUnknown(s.env.AIProvider), orUnknown(s.models.defaultID))
	fmt.Fprintf(&builder, "Repository: %s (default branch %s), GitHub tools %s\n", orUnknown(s.env.Repository), orUnknown(s.env.DefaultBranch), enabledText(s.github != nil))
	fmt.Fprintf(&builder, "Records: %s\n", s.instanceSnapshot(ctx))
	if s.logs != nil {
		problems := s.logs.Snapshot(maxLogLines, isProblemLine)
		fmt.Fprintf(&builder, "Recent problems in the log buffer: %d\n", len(problems))
		if len(problems) > 0 {
			fmt.Fprintf(&builder, "Latest: %s\n", clipText(problems[len(problems)-1], maxLogLineWidth))
		}
	}
	return builder.String()
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func enabledText(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func isProblemLine(line string) bool {
	if logLevelPattern.MatchString(line) || serverStatusPattern.MatchString(line) {
		return true
	}
	return strings.Contains(line, `"event":"legacy_log"`) && legacyProblemPattern.MatchString(line)
}

func (s *Service) readLogs(args toolArgs) (string, error) {
	if s.logs == nil {
		return "", errToolNotReady
	}
	limit := max(1, min(args.number("limit", defaultLogLines), maxLogLines))
	filter := strings.ToLower(args.trimmed("contains"))
	problemsOnly := !strings.EqualFold(args.trimmed("kind"), "all")
	lines := s.logs.Snapshot(limit, func(line string) bool {
		if problemsOnly && !isProblemLine(line) {
			return false
		}
		return filter == "" || strings.Contains(strings.ToLower(line), filter)
	})
	if len(lines) == 0 {
		return "No matching log lines in the in-memory buffer. It holds the newest few thousand lines since the last restart.", nil
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d log line(s), oldest first\n", len(lines))
	for _, line := range lines {
		builder.WriteString(clipText(line, maxLogLineWidth) + "\n")
	}
	return builder.String(), nil
}

// readOnlyQuery runs one SELECT. Defense in depth: a keyword screen, a
// single-statement rule, a read-only transaction on PostgreSQL, a statement
// timeout, and a rollback that always happens.
func (s *Service) readOnlyQuery(ctx context.Context, args toolArgs) (string, error) {
	statement := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(args.text("sql")), ";"))
	lower := strings.ToLower(statement)
	switch {
	case statement == "":
		return "", errors.New("sql is required")
	case !strings.HasPrefix(lower, "select") && !strings.HasPrefix(lower, "with"):
		return "", errors.New("only SELECT statements are allowed")
	case strings.Contains(statement, ";"):
		return "", errors.New("run one statement at a time, without semicolons")
	case forbiddenSQLPattern.MatchString(lower):
		return "", errors.New("the statement contains a keyword that is not allowed in a read-only query")
	}
	limit := max(1, min(args.number("limit", defaultQueryRows), maxQueryRows))

	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("could not start a read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if strings.Contains(strings.ToLower(s.env.DatabaseDriver), "postgres") {
		if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
			return "", fmt.Errorf("could not make the transaction read-only: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", queryTimeoutMs)); err != nil {
			return "", fmt.Errorf("could not set a statement timeout: %w", err)
		}
	}
	rows, err := tx.QueryContext(ctx, statement)
	if err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()
	result, err := formatQueryRows(rows, limit)
	if err != nil {
		return "", err
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}
	return result, nil
}

func formatQueryRows(rows *sql.Rows, limit int) (string, error) {
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	masked := make([]bool, len(columns))
	for index, name := range columns {
		masked[index] = sensitiveColumnPattern.MatchString(name)
	}
	var builder strings.Builder
	builder.WriteString(strings.Join(columns, "\t") + "\n")
	count := 0
	for rows.Next() {
		if count == limit {
			fmt.Fprintf(&builder, "…more rows exist; only the first %d are shown. Add a WHERE clause or a smaller limit.\n", limit)
			break
		}
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return "", fmt.Errorf("could not read a row: %w", err)
		}
		cells := make([]string, len(columns))
		for index, value := range values {
			if masked[index] {
				cells[index] = "[masked]"
				continue
			}
			cells[index] = queryCell(value)
		}
		builder.WriteString(strings.Join(cells, "\t") + "\n")
		count++
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}
	if count == 0 {
		builder.WriteString("(no rows)\n")
	}
	return builder.String(), nil
}

func queryCell(value any) string {
	switch typed := value.(type) {
	case nil:
		return "NULL"
	case []byte:
		if !utf8.Valid(typed) {
			return fmt.Sprintf("[binary %d bytes]", len(typed))
		}
		return flattenCell(string(typed))
	case time.Time:
		return typed.UTC().Format(time.RFC3339)
	case string:
		return flattenCell(typed)
	default:
		return flattenCell(fmt.Sprint(typed))
	}
}

func flattenCell(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxQueryCellWidth {
		return clipText(text, maxQueryCellWidth)
	}
	return text
}

func (s *Service) instanceSnapshot(ctx context.Context) string {
	count := func(table string) int {
		var total int
		_ = s.db.NewSelect().Table(table).ColumnExpr("COUNT(*)").Scan(ctx, &total)
		return total
	}
	return fmt.Sprintf("%d users, %d workspaces, %d publications", count("users"), count("workspaces"), count("publications"))
}
