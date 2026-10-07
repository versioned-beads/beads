// Package doltcli is the only place the replay harness starts dolt. Every
// query goes through one runner with one environment deny-list, refuses to
// query a directory a dolt sql-server is serving, and treats stdout as data
// and stderr as diagnostics only.
package doltcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Cell is one field of a dolt sql csv result. dolt writes SQL NULL as a bare
// empty field and the empty string as a quoted one, so a field is NULL exactly
// when it was unquoted and empty. Text is the field's value with the quoting
// removed, and is empty for NULL.
type Cell struct {
	Null bool
	Text string
}

// deniedEnvVars are ambient environment variables that must never reach a
// dolt or bd process the harness starts: letting one through risks silently
// routing a replay at a shared or production store, or attaching the wrong
// actor identity to a mutation the harness makes on its own behalf. The last
// three are the product's own switches for its safety checks (the remote-backed
// migration gate, the workspace identity check and the schema-skew check): a
// harness that let one through would be testing a product with its guards off.
var deniedEnvVars = map[string]bool{
	"BEADS_DOLT_SERVER_PORT":      true,
	"BEADS_DOLT_PORT":             true,
	"BEADS_ACTOR":                 true,
	"BD_ACTOR":                    true,
	"GT_ROOT":                     true,
	"BEADS_DIR":                   true,
	"BEADS_HOLDER_TOKEN":          true,
	"GC_BEADS_SCOPE_ROOT":         true,
	"BEADS_DOLT_AUTO_START":       true,
	"BEADS_DOLT_SYNC_CLI_REMOTES": true,
	"BEADS_BACKUP_ENABLED":        true,
	"BD_ALLOW_REMOTE_MIGRATE":     true,
	"BEADS_SKIP_IDENTITY_CHECK":   true,
	"BD_IGNORE_SCHEMA_SKEW":       true,
}

// DeniedDoltVerbs are the dolt subcommands that send a store's state somewhere
// else or take someone else's state in: each starts with the words listed. The
// harness's Runner refuses every one of them. Taking a remote or a backup away
// (`remote remove`, `backup remove`) is not on the list: that is how a seeded
// copy is cut off from the places it was cloned from.
var DeniedDoltVerbs = [][]string{
	{"push"}, {"fetch"}, {"pull"},
	{"remote", "add"},
	{"backup", "add"}, {"backup", "sync"}, {"backup", "sync-url"}, {"backup", "restore"},
}

// DeniedBdVerbs are the bd subcommands that do the same through bd, which has its
// own way to push a store, pull one and back one up. Every `bd backup` verb is
// refused, whatever follows it.
var DeniedBdVerbs = [][]string{
	{"backup"},
	{"dolt", "push"}, {"dolt", "pull"},
}

// DeniedSQLProcedures are the stored procedures that do from inside a statement
// what the dolt verbs above do from the command line. A `dolt sql` command whose
// statement calls one of them is refused like the verb it stands for. Reading the
// tables that list remotes and backups is not a call and is allowed.
var DeniedSQLProcedures = []string{"dolt_push", "dolt_pull", "dolt_fetch", "dolt_remote", "dolt_backup"}

// SanitizedEnv returns base with every denied ambient variable removed, for
// use as the environment of any dolt or bd process the harness starts.
func SanitizedEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if deniedEnvVars[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Path returns the dolt binary the harness runs, looked up on PATH at call
// time so a test can put a stand-in ahead of it. It is the one place dolt is
// located: a caller that only needs to know whether dolt is available asks the
// same question the runner does.
func Path() (string, error) {
	return exec.LookPath("dolt")
}

// command prepares a dolt invocation in dir, started with the sanitized
// environment.
func command(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	doltPath, err := Path()
	if err != nil {
		return nil, fmt.Errorf("doltcli: %w", err)
	}
	cmd := exec.CommandContext(ctx, doltPath, args...)
	cmd.Dir = dir
	cmd.Env = SanitizedEnv(os.Environ())
	return cmd, nil
}

// Query runs one read-only SQL statement in the dolt database at dir and
// returns the CSV header and the data rows. It issues only the statement it
// is given, through `dolt sql -q ... -r csv`; dir must be a local clone, never
// a directory a dolt sql-server is serving. Only stdout is parsed: stderr is
// reported in the error and never mistaken for data.
//
// Rows come back as cells, not strings, because the csv text says more than its
// values: NULL and the empty string differ only in whether the field was quoted.
// Never read rows with `-r json` instead: it leaves NULL columns out of the row
// and rounds numbers inside JSON columns.
func Query(ctx context.Context, dir, sql string) (header []string, rows [][]Cell, err error) {
	if err := refuseServed(dir); err != nil {
		return nil, nil, err
	}
	cmd, err := command(ctx, dir, "sql", "-q", sql, "-r", "csv")
	if err != nil {
		return nil, nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("dolt sql: %w: %s", err, stderr.String())
	}
	header, rows, err = parseCSV(out)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing dolt sql csv output: %w", err)
	}
	return header, rows, nil
}

// parseCSV reads the csv text dolt writes into a header and cells. It is an
// RFC 4180 reader that keeps what encoding/csv throws away, whether a field was
// quoted, because that is the only thing separating NULL from the empty string.
// Records end at LF or CRLF outside quotes, inside quotes a newline is data, and
// every record must have as many fields as the header.
//
// A result of one column shows NULL as an empty line, so an empty line is a
// record of one NULL field rather than a blank to skip; under a wider header it
// is a field-count error like any other short record. Nothing after the final
// newline is a record.
func parseCSV(data []byte) (header []string, rows [][]Cell, err error) {
	p := csvParser{data: data}
	var records [][]Cell
	for p.pos < len(p.data) {
		rec, err := p.record()
		if err != nil {
			return nil, nil, fmt.Errorf("record %d: %w", len(records)+1, err)
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return nil, nil, nil
	}
	header = make([]string, len(records[0]))
	for i, c := range records[0] {
		header[i] = c.Text
	}
	for i, rec := range records[1:] {
		if len(rec) != len(header) {
			return nil, nil, fmt.Errorf("record %d has %d fields, the header has %d", i+2, len(rec), len(header))
		}
	}
	return header, records[1:], nil
}

type csvParser struct {
	data []byte
	pos  int
}

// record reads one record: fields up to a newline outside quotes, or to the end
// of the data.
func (p *csvParser) record() ([]Cell, error) {
	var rec []Cell
	for {
		cell, sep, err := p.field()
		if err != nil {
			return nil, err
		}
		rec = append(rec, cell)
		if sep != ',' {
			return rec, nil
		}
	}
}

// field reads one field and the separator that ended it: ',' , '\n' (a CRLF is
// one newline) or 0 at the end of the data.
func (p *csvParser) field() (Cell, byte, error) {
	if p.pos < len(p.data) && p.data[p.pos] == '"' {
		p.pos++
		var text []byte
		for {
			i := bytes.IndexByte(p.data[p.pos:], '"')
			if i < 0 {
				return Cell{}, 0, errors.New("unterminated quoted field")
			}
			text = append(text, p.data[p.pos:p.pos+i]...)
			p.pos += i + 1
			if p.pos < len(p.data) && p.data[p.pos] == '"' {
				text = append(text, '"')
				p.pos++
				continue
			}
			break
		}
		sep, err := p.separator()
		if err != nil {
			return Cell{}, 0, err
		}
		return Cell{Text: string(text)}, sep, nil
	}

	start := p.pos
	for p.pos < len(p.data) && p.data[p.pos] != ',' && p.data[p.pos] != '\n' {
		if p.data[p.pos] == '"' {
			return Cell{}, 0, errors.New("bare quote in an unquoted field")
		}
		p.pos++
	}
	text := p.data[start:p.pos]
	var sep byte
	if p.pos < len(p.data) {
		sep = p.data[p.pos]
		p.pos++
		if sep == '\n' {
			text = bytes.TrimSuffix(text, []byte{'\r'})
		}
	}
	if len(text) == 0 {
		return Cell{Null: true}, sep, nil
	}
	return Cell{Text: string(text)}, sep, nil
}

// separator reads what must follow a closing quote: a comma, a newline or the
// end of the data.
func (p *csvParser) separator() (byte, error) {
	if p.pos >= len(p.data) {
		return 0, nil
	}
	switch p.data[p.pos] {
	case ',':
		p.pos++
		return ',', nil
	case '\n':
		p.pos++
		return '\n', nil
	case '\r':
		if p.pos+1 < len(p.data) && p.data[p.pos+1] == '\n' {
			p.pos += 2
			return '\n', nil
		}
	}
	return 0, errors.New("text after a closing quote")
}

// Run runs an arbitrary dolt subcommand in dir and returns its combined
// output. It is for fixtures and other setup that must change a database; it
// does not apply the served-directory guard.
func Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd, err := command(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("dolt %s (dir=%s): %w\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out, nil
}

// RowMap builds a column-name to text map from one CSV row and its header. It
// is the text-only view of a row: NULL and the empty string both read as "", so
// use it only where that difference does not matter and read the cells directly
// where it does.
func RowMap(header []string, row []Cell) map[string]string {
	m := make(map[string]string, len(header))
	for i, h := range header {
		if i < len(row) {
			m[h] = row[i].Text
		}
	}
	return m
}

// sqlEscaper doubles the two characters that mean something inside a
// single-quoted dolt string literal. dolt reads a backslash there as an escape, so
// a lone one before a doubled quote would escape the first of the pair and end the
// literal on the second.
var sqlEscaper = strings.NewReplacer(`\`, `\\`, `'`, `''`)

// SQLQuote wraps a value in single quotes for embedding in a dolt sql -q
// statement, doubling any quote or backslash inside it. dolt sql -q has no
// bind-parameter API, so callers still validate refs and ids before they get
// here; this keeps a value they miss from ending the literal.
func SQLQuote(s string) string {
	return "'" + sqlEscaper.Replace(s) + "'"
}

// refuseServed is the error for a query aimed at a database a dolt sql-server is
// serving, and nil for any other: a dolt command run there would route through the
// server instead of reading the local clone.
func refuseServed(dir string) error {
	if served := servedDataDir(dir); served != "" {
		return fmt.Errorf("doltcli: %s is under %s, which a dolt sql-server is serving; querying it would route through that server instead of the local clone", dir, served)
	}
	return nil
}

// servedDataDir returns dir or its nearest ancestor that a dolt sql-server is
// serving, identified by the .dolt/sql-server.info record the server writes at
// startup, or "" when there is none. The dolt CLI routes commands for any
// database under such a directory through that server.
func servedDataDir(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if recordsLiveServer(filepath.Join(dir, ".dolt", "sql-server.info")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// recordsLiveServer reports whether the sql-server.info record at path exists
// and does not name a process that has exited. A server writes "pid:port:id" at
// startup and does not remove the record if it dies, so a record whose pid is
// gone is stale and must not refuse every later query in that directory. A
// record that cannot be read as a pid cannot be proven stale, so it still counts
// as a server.
func recordsLiveServer(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is a fixed .dolt/sql-server.info under a directory the caller chose to query
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	pidText, _, _ := strings.Cut(strings.TrimSpace(string(data)), ":")
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return true
	}
	return processAlive(pid)
}
