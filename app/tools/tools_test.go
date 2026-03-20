package tools

import (
	"database/sql"
	"encoding/json"
	"math"
	"net"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"next/app/types"
)

// ---------------------------------------------------------------------------
// TestBuildToolDefinition
// ---------------------------------------------------------------------------

func TestBuildToolDefinition(t *testing.T) {
	tests := []struct {
		name       string
		ct         types.CustomTool
		wantName   string
		wantDesc   string
		wantReqLen int
	}{
		{
			name: "basic tool with required param",
			ct: types.CustomTool{
				Name:        "my_tool",
				Description: "A test tool",
				Parameters:  `[{"name":"query","type":"string","description":"search term","required":true}]`,
			},
			wantName:   "my_tool",
			wantDesc:   "A test tool",
			wantReqLen: 1,
		},
		{
			name: "tool with no required params",
			ct: types.CustomTool{
				Name:        "optional_tool",
				Description: "Optional params only",
				Parameters:  `[{"name":"limit","type":"integer","description":"max results","required":false}]`,
			},
			wantName:   "optional_tool",
			wantDesc:   "Optional params only",
			wantReqLen: 0,
		},
		{
			name: "tool with empty parameters",
			ct: types.CustomTool{
				Name:        "empty_tool",
				Description: "No params",
				Parameters:  `[]`,
			},
			wantName:   "empty_tool",
			wantDesc:   "No params",
			wantReqLen: 0,
		},
		{
			name: "tool with default type (empty type becomes string)",
			ct: types.CustomTool{
				Name:        "default_type",
				Description: "Param with empty type",
				Parameters:  `[{"name":"val","type":"","description":"a value","required":true}]`,
			},
			wantName:   "default_type",
			wantDesc:   "Param with empty type",
			wantReqLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := BuildToolDefinition(tc.ct)
			if tool.Function == nil {
				t.Fatal("Function is nil")
			}
			if tool.Function.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", tool.Function.Name, tc.wantName)
			}
			if tool.Function.Description != tc.wantDesc {
				t.Errorf("Description = %q, want %q", tool.Function.Description, tc.wantDesc)
			}

			var schema map[string]any
			if err := json.Unmarshal(tool.Function.Parameters.(json.RawMessage), &schema); err != nil {
				t.Fatalf("unmarshal parameters: %v", err)
			}
			req, _ := schema["required"].([]any)
			if len(req) != tc.wantReqLen {
				t.Errorf("required len = %d, want %d", len(req), tc.wantReqLen)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestExtractJSONPath
// ---------------------------------------------------------------------------

func TestExtractJSONPath(t *testing.T) {
	tests := []struct {
		name     string
		jsonStr  string
		path     string
		expected string
	}{
		{
			name:     "empty path returns original",
			jsonStr:  `{"a":1}`,
			path:     "",
			expected: `{"a":1}`,
		},
		{
			name:     "simple key",
			jsonStr:  `{"name":"Alice"}`,
			path:     "name",
			expected: "Alice",
		},
		{
			name:     "nested key",
			jsonStr:  `{"data":{"value":42}}`,
			path:     "data.value",
			expected: "42",
		},
		{
			name:     "array index",
			jsonStr:  `{"items":["a","b","c"]}`,
			path:     "items.1",
			expected: "b",
		},
		{
			name:     "nested object in array",
			jsonStr:  `{"results":[{"id":1},{"id":2}]}`,
			path:     "results.1.id",
			expected: "2",
		},
		{
			name:     "missing key returns original",
			jsonStr:  `{"a":1}`,
			path:     "b",
			expected: `{"a":1}`,
		},
		{
			name:     "invalid json returns original",
			jsonStr:  `not json`,
			path:     "key",
			expected: `not json`,
		},
		{
			name:     "out of bounds array returns original",
			jsonStr:  `{"items":[1,2]}`,
			path:     "items.5",
			expected: `{"items":[1,2]}`,
		},
		{
			name:     "string value at leaf",
			jsonStr:  `{"a":{"b":"hello"}}`,
			path:     "a.b",
			expected: "hello",
		},
		{
			name:     "object value at leaf marshals to JSON",
			jsonStr:  `{"a":{"b":{"c":1}}}`,
			path:     "a.b",
			expected: `{"c":1}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractJSONPath(tc.jsonStr, tc.path)
			if got != tc.expected {
				t.Errorf("ExtractJSONPath(%q, %q) = %q, want %q", tc.jsonStr, tc.path, got, tc.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestValidateReadOnlySQL
// ---------------------------------------------------------------------------

func TestValidateReadOnlySQL(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantErr bool
		errMsg  string
	}{
		{"valid select", "SELECT * FROM messages", false, ""},
		{"valid with", "WITH cte AS (SELECT 1) SELECT * FROM cte", false, ""},
		{"empty query", "", true, "query vazia"},
		{"insert blocked", "INSERT INTO messages VALUES (1)", true, "apenas SELECT permitido"},
		{"delete blocked", "DELETE FROM messages", true, "apenas SELECT permitido"},
		{"drop blocked", "DROP TABLE messages", true, "apenas SELECT permitido"},
		{"update blocked", "UPDATE messages SET content='x'", true, "apenas SELECT permitido"},
		{"multiple statements", "SELECT 1; DROP TABLE messages", true, "multiplos comandos nao permitidos"},
		{"trailing semicolon ok", "SELECT 1;", false, ""},
		{"blocked table config", "SELECT * FROM config", true, "tabela bloqueada: config"},
		{"blocked table custom_tools", "SELECT * FROM custom_tools", true, "tabela bloqueada: custom_tools"},
		{"blocked table mcp_servers", "SELECT * FROM mcp_servers", true, "tabela bloqueada: mcp_servers"},
		{"blocked table external_databases", "SELECT * FROM external_databases", true, "tabela bloqueada: external_databases"},
		{"non-select start", "EXPLAIN SELECT 1", true, "apenas SELECT permitido"},
		{"write keyword in select", "SELECT * FROM messages; DELETE FROM messages", true, "multiplos comandos nao permitidos"},
		{"insert keyword in subquery", "SELECT * FROM (INSERT INTO messages VALUES (1))", true, "palavra-chave bloqueada: INSERT"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReadOnlySQL(tc.query)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errMsg)
				} else if tc.errMsg != "" && err.Error() != tc.errMsg {
					t.Errorf("error = %q, want %q", err.Error(), tc.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestValidateExtReadOnlySQL
// ---------------------------------------------------------------------------

func TestValidateExtReadOnlySQL(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{"valid select", "SELECT * FROM users", false},
		{"valid with CTE", "WITH x AS (SELECT 1) SELECT * FROM x", false},
		{"empty query", "", true},
		{"insert blocked", "INSERT INTO users VALUES (1)", true},
		{"multiple statements", "SELECT 1; SELECT 2", true},
		{"trailing semicolon ok", "SELECT 1;", false},
		// Unlike ValidateReadOnlySQL, external DB validation does NOT block specific tables
		{"config table allowed in ext", "SELECT * FROM config", false},
		{"custom_tools allowed in ext", "SELECT * FROM custom_tools", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExtReadOnlySQL(tc.query)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestCalcEval
// ---------------------------------------------------------------------------

func TestCalcEval(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		want    float64
		wantErr bool
	}{
		{"addition", "2 + 3", 5, false},
		{"subtraction", "10 - 4", 6, false},
		{"multiplication", "3 * 7", 21, false},
		{"division", "20 / 4", 5, false},
		{"modulo", "10 % 3", 1, false},
		{"power", "2 ^ 10", 1024, false},
		{"parentheses", "(2 + 3) * 4", 20, false},
		{"nested parentheses", "((1 + 2) * (3 + 4))", 21, false},
		{"unary minus", "-5 + 3", -2, false},
		{"unary plus", "+5", 5, false},
		{"complex expression", "2 + 3 * 4 - 1", 13, false},
		{"sqrt function", "sqrt(144)", 12, false},
		{"abs function", "abs(-42)", 42, false},
		{"floor function", "floor(3.7)", 3, false},
		{"ceil function", "ceil(3.2)", 4, false},
		{"round function", "round(3.5)", 4, false},
		{"decimal number", "1.5 + 2.5", 4, false},
		{"division by zero", "1 / 0", 0, true},
		{"empty expression", "", 0, false}, // parseAtom returns 0 for empty
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := calcEval(tc.expr)
			if tc.wantErr {
				if err == nil {
					t.Errorf("calcEval(%q) expected error, got %v", tc.expr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("calcEval(%q) unexpected error: %v", tc.expr, err)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("calcEval(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestBuildDSN
// ---------------------------------------------------------------------------

func TestBuildDSN(t *testing.T) {
	tests := []struct {
		name string
		edb  types.ExternalDatabase
		want string
	}{
		{
			name: "mysql basic",
			edb: types.ExternalDatabase{
				Driver:   "mysql",
				Host:     "localhost",
				Port:     3306,
				Username: "root",
				Password: "pass",
				DBName:   "mydb",
				SSLMode:  "disable",
			},
			want: "root:pass@tcp(localhost:3306)/mydb?tls=false&timeout=5s&readTimeout=10s&parseTime=true",
		},
		{
			name: "mysql with ssl require",
			edb: types.ExternalDatabase{
				Driver:   "mysql",
				Host:     "db.example.com",
				Port:     3306,
				Username: "user",
				Password: "secret",
				DBName:   "prod",
				SSLMode:  "require",
			},
			want: "user:secret@tcp(db.example.com:3306)/prod?tls=true&timeout=5s&readTimeout=10s&parseTime=true",
		},
		{
			name: "mysql with ssl preferred",
			edb: types.ExternalDatabase{
				Driver:   "mysql",
				Host:     "db.example.com",
				Port:     3306,
				Username: "user",
				Password: "pass",
				DBName:   "mydb",
				SSLMode:  "preferred",
			},
			want: "user:pass@tcp(db.example.com:3306)/mydb?tls=preferred&timeout=5s&readTimeout=10s&parseTime=true",
		},
		{
			name: "postgres basic",
			edb: types.ExternalDatabase{
				Driver:   "postgres",
				Host:     "localhost",
				Port:     5432,
				Username: "pguser",
				Password: "pgpass",
				DBName:   "pgdb",
				SSLMode:  "disable",
			},
			want: "host=localhost port=5432 user=pguser password=pgpass dbname=pgdb sslmode=disable connect_timeout=5",
		},
		{
			name: "unknown driver returns empty",
			edb: types.ExternalDatabase{
				Driver: "oracle",
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildDSN(tc.edb)
			if got != tc.want {
				t.Errorf("BuildDSN() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestValidateExternalDatabase
// ---------------------------------------------------------------------------

func TestValidateExternalDatabase(t *testing.T) {
	valid := types.ExternalDatabase{
		Name:   "mydb",
		Driver: "postgres",
		Host:   "localhost",
		Port:   5432,
		DBName: "testdb",
	}

	tests := []struct {
		name    string
		modify  func(types.ExternalDatabase) types.ExternalDatabase
		wantErr bool
		errMsg  string
	}{
		{"valid config", func(e types.ExternalDatabase) types.ExternalDatabase { return e }, false, ""},
		{"empty name", func(e types.ExternalDatabase) types.ExternalDatabase { e.Name = ""; return e }, true, "nome obrigatorio"},
		{"invalid name chars", func(e types.ExternalDatabase) types.ExternalDatabase { e.Name = "My DB!"; return e }, true, "nome invalido: use apenas letras minusculas, numeros e _ (comece com letra, max 64 chars)"},
		{"reserved name local", func(e types.ExternalDatabase) types.ExternalDatabase { e.Name = "local"; return e }, true, "nome 'local' e reservado"},
		{"invalid driver", func(e types.ExternalDatabase) types.ExternalDatabase { e.Driver = "oracle"; return e }, true, "driver deve ser 'mysql' ou 'postgres'"},
		{"mysql driver ok", func(e types.ExternalDatabase) types.ExternalDatabase { e.Driver = "mysql"; return e }, false, ""},
		{"empty host", func(e types.ExternalDatabase) types.ExternalDatabase { e.Host = ""; return e }, true, "host obrigatorio"},
		{"port zero", func(e types.ExternalDatabase) types.ExternalDatabase { e.Port = 0; return e }, true, "porta deve ser entre 1 e 65535"},
		{"port too high", func(e types.ExternalDatabase) types.ExternalDatabase { e.Port = 70000; return e }, true, "porta deve ser entre 1 e 65535"},
		{"empty dbname", func(e types.ExternalDatabase) types.ExternalDatabase { e.DBName = ""; return e }, true, "nome do banco obrigatorio"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edb := tc.modify(valid)
			err := ValidateExternalDatabase(edb)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errMsg)
				} else if tc.errMsg != "" && err.Error() != tc.errMsg {
					t.Errorf("error = %q, want %q", err.Error(), tc.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestReToolName
// ---------------------------------------------------------------------------

func TestReToolName(t *testing.T) {
	tests := []struct {
		input string
		match bool
	}{
		{"my_tool", true},
		{"a", true},
		{"tool123", true},
		{"a_b_c", true},
		{"z0123456789", true},
		{"", false},
		{"MyTool", false},                 // uppercase not allowed
		{"1tool", false},                  // starts with number
		{"_tool", false},                  // starts with underscore
		{"tool-name", false},              // hyphen not allowed
		{"tool name", false},              // space not allowed
		{"tool!", false},                  // special char
		{"Tool", false},                   // uppercase
		{string(make([]byte, 65)), false}, // too long (65 chars)
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := ReToolName.MatchString(tc.input)
			if got != tc.match {
				t.Errorf("ReToolName.MatchString(%q) = %v, want %v", tc.input, got, tc.match)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestValidateAgent
// ---------------------------------------------------------------------------

func TestValidateAgent(t *testing.T) {
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Create agents table
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS agents (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		name          TEXT NOT NULL UNIQUE,
		description   TEXT NOT NULL DEFAULT '',
		system_prompt TEXT NOT NULL,
		user_prompt   TEXT NOT NULL DEFAULT '',
		model         TEXT NOT NULL DEFAULT 'gpt-4.1-mini',
		max_tokens    INTEGER NOT NULL DEFAULT 1024,
		base_url      TEXT NOT NULL DEFAULT '',
		api_key       TEXT NOT NULL DEFAULT '',
		enabled       INTEGER NOT NULL DEFAULT 1,
		chain_to      INTEGER NOT NULL DEFAULT 0,
		chain_condition TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL DEFAULT (unixepoch())
	)`)
	if err != nil {
		t.Fatal(err)
	}

	validAgent := types.Agent{
		Name:         "Test Agent",
		SystemPrompt: "You are helpful",
		Model:        "gpt-4.1-mini",
		MaxTokens:    1024,
	}

	tests := []struct {
		name    string
		modify  func(types.Agent) types.Agent
		editID  int64
		wantErr bool
		errMsg  string
	}{
		{"valid agent", func(a types.Agent) types.Agent { return a }, 0, false, ""},
		{"invalid name", func(a types.Agent) types.Agent { a.Name = "!!!"; return a }, 0, true, "Nome invalido"},
		{"empty system prompt", func(a types.Agent) types.Agent { a.SystemPrompt = ""; return a }, 0, true, "System prompt obrigatorio"},
		{"empty model", func(a types.Agent) types.Agent { a.Model = ""; return a }, 0, true, "Modelo obrigatorio"},
		{"max_tokens too low", func(a types.Agent) types.Agent { a.MaxTokens = 10; return a }, 0, true, "Max tokens deve ser entre 50 e 32000"},
		{"max_tokens too high", func(a types.Agent) types.Agent { a.MaxTokens = 50000; return a }, 0, true, "Max tokens deve ser entre 50 e 32000"},
		{"invalid base_url", func(a types.Agent) types.Agent { a.BaseURL = "ftp://bad"; return a }, 0, true, "Base URL deve comecar com http:// ou https://"},
		{"valid base_url http", func(a types.Agent) types.Agent { a.BaseURL = "http://localhost:8080"; return a }, 0, false, ""},
		{"valid base_url https", func(a types.Agent) types.Agent { a.BaseURL = "https://api.example.com"; return a }, 0, false, ""},
		{"empty base_url ok", func(a types.Agent) types.Agent { a.BaseURL = ""; return a }, 0, false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := tc.modify(validAgent)
			err := ValidateAgent(agent, db, tc.editID)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errMsg)
				} else if tc.errMsg != "" && !contains(err.Error(), tc.errMsg) {
					t.Errorf("error = %q, want containing %q", err.Error(), tc.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}

	// Test agent limit (10 max)
	t.Run("agent limit reached", func(t *testing.T) {
		// Insert 10 agents
		for i := 0; i < 10; i++ {
			_, err := db.Exec("INSERT INTO agents (name, system_prompt, model, max_tokens) VALUES (?, 'prompt', 'model', 1024)",
				"Agent"+string(rune('A'+i)))
			if err != nil {
				t.Fatal(err)
			}
		}

		err := ValidateAgent(validAgent, db, 0)
		if err == nil {
			t.Error("expected agent limit error, got nil")
		} else if !contains(err.Error(), "Limite de 10 agentes") {
			t.Errorf("error = %q, want containing 'Limite de 10 agentes'", err.Error())
		}
	})
}

// contains checks if s contains substr (simple helper to avoid importing strings in test).
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// TestStripSQLComments
// ---------------------------------------------------------------------------

func TestStripSQLComments_LineComment(t *testing.T) {
	got := stripSQLComments("SELECT 1 -- comment")
	want := "SELECT 1 "
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripSQLComments_BlockComment(t *testing.T) {
	got := stripSQLComments("SELECT /* hidden */ 1")
	// Block comment is replaced with a single space, preserving surrounding spaces
	want := "SELECT  " + " 1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripSQLComments_NoComments(t *testing.T) {
	input := "SELECT * FROM messages WHERE id = 1"
	got := stripSQLComments(input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestStripSQLComments_MultiLine(t *testing.T) {
	input := "SELECT * FROM messages -- line comment\nWHERE /* block */ id = 1"
	got := stripSQLComments(input)
	// Block comment "/* block */" replaced with single space, so "WHERE " + " " + " id" = 3 spaces.
	// Line comment stripped at "--".
	want := "SELECT * FROM messages \nWHERE   id = 1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// TestValidateReadOnlySQL (additional cases for sqlite_master/schema and comment bypass)
// ---------------------------------------------------------------------------

func TestValidateReadOnlySQL_SqliteMaster(t *testing.T) {
	err := ValidateReadOnlySQL("SELECT * FROM sqlite_master")
	if err == nil {
		t.Fatal("expected error for sqlite_master, got nil")
	}
}

func TestValidateReadOnlySQL_SqliteSchema(t *testing.T) {
	err := ValidateReadOnlySQL("SELECT * FROM sqlite_schema")
	if err == nil {
		t.Fatal("expected error for sqlite_schema, got nil")
	}
}

func TestValidateReadOnlySQL_CommentBypass(t *testing.T) {
	// "config" is a blocked table; a line comment after it should not bypass the check
	err := ValidateReadOnlySQL("SELECT * FROM config -- hidden")
	if err == nil {
		t.Fatal("expected error for blocked table config with line comment, got nil")
	}
}

func TestValidateReadOnlySQL_BlockCommentBypass(t *testing.T) {
	// "config" appears only inside a block comment, not in the actual query
	err := ValidateReadOnlySQL("SELECT * /* from config */ FROM messages")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestValidateURLSafety
// ---------------------------------------------------------------------------

func TestValidateURLSafety_PublicURL(t *testing.T) {
	err := validateURLSafety("https://api.example.com/data")
	if err != nil {
		// The function does DNS resolution; skip if DNS fails in test env
		if dnsErr, ok := err.(*net.DNSError); ok {
			t.Skipf("skipping due to DNS error: %v", dnsErr)
		}
		// Also skip if the error message indicates a lookup failure
		if strings.Contains(err.Error(), "lookup") || strings.Contains(err.Error(), "DNS") || strings.Contains(err.Error(), "no such host") {
			t.Skipf("skipping due to DNS resolution failure: %v", err)
		}
		t.Errorf("unexpected error for public URL: %v", err)
	}
}

func TestValidateURLSafety_Localhost(t *testing.T) {
	err := validateURLSafety("http://localhost:8080")
	if err == nil {
		t.Fatal("expected error for localhost URL, got nil")
	}
}

func TestValidateURLSafety_PrivateIP(t *testing.T) {
	err := validateURLSafety("http://192.168.1.1")
	if err == nil {
		t.Fatal("expected error for private IP URL, got nil")
	}
}

func TestValidateURLSafety_Loopback(t *testing.T) {
	err := validateURLSafety("http://127.0.0.1")
	if err == nil {
		t.Fatal("expected error for loopback URL, got nil")
	}
}
