package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/database"
)

type ConnectionService interface {
	List(context.Context, contracts.UserID) ([]database.Connection, error)
	Get(context.Context, contracts.UserID, contracts.DatabaseID) (database.Connection, error)
	Create(context.Context, contracts.UserID, database.ConnectionInput) (database.Connection, error)
	Update(context.Context, contracts.UserID, contracts.DatabaseID, database.ConnectionInput) (database.Connection, error)
	Delete(context.Context, contracts.UserID, contracts.DatabaseID) error
	TestConnection(context.Context, contracts.UserID, contracts.DatabaseID) error
	Accessible(context.Context, contracts.UserID) ([]database.DatabaseChoice, error)
	Describe(context.Context, contracts.UserID, contracts.DatabaseID) (database.DatabaseChoice, error)
}

type DatabaseViewer interface {
	Schemas(context.Context, contracts.UserID, contracts.DatabaseID) ([]string, error)
	Tables(context.Context, contracts.UserID, contracts.DatabaseID, string) ([]database.Table, error)
	Details(context.Context, contracts.UserID, contracts.DatabaseID, string, string) (database.TableDetails, error)
	Data(context.Context, contracts.UserID, contracts.DatabaseID, string, string, int) (database.DataPage, error)
	Diff(context.Context, contracts.UserID, contracts.DatabaseID, string, string) (database.SchemaDiff, error)
}

type databaseHandler struct {
	connections ConnectionService
	viewer      DatabaseViewer
	authorizer  interface {
		RequirePlatformAdmin(context.Context, contracts.UserID) error
	}
}

func NewDatabaseHandler(connections ConnectionService, viewer DatabaseViewer, authorizer interface {
	RequirePlatformAdmin(context.Context, contracts.UserID) error
}, sessions Sessions) (http.Handler, error) {
	if connections == nil || viewer == nil || authorizer == nil || sessions == nil {
		return nil, errors.New("database API requires connections, viewer, authorizer, and sessions")
	}
	h := &databaseHandler{connections, viewer, authorizer}
	return sessions.RequireSessionWithError(http.HandlerFunc(h.serve), func(w http.ResponseWriter, _ *http.Request, err error) {
		if errors.Is(err, auth.ErrUnauthenticated) {
			WriteError(w, 401, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
			return
		}
		WriteError(w, 503, "AUTH_UNAVAILABLE", "Layanan login sementara tidak tersedia.")
	}), nil
}

type connectionInput struct {
	Name     string  `json:"name"`
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Database string  `json:"database"`
	Username string  `json:"username"`
	SSLMode  string  `json:"ssl_mode"`
	Password *string `json:"password"`
}

func (in connectionInput) value() database.ConnectionInput {
	return database.ConnectionInput{Name: in.Name, Host: in.Host, Port: in.Port, Database: in.Database, Username: in.Username, SSLMode: in.SSLMode, Password: in.Password}
}

func validateConnection(w http.ResponseWriter, in connectionInput, create bool) bool {
	fields := in.value().ValidationFields(create)
	if len(fields) == 0 {
		return true
	}
	writeJSON(w, 422, struct {
		Error errorDetail `json:"error"`
	}{errorDetail{Code: "VALIDATION_FAILED", Message: "Isian tidak valid.", FieldErrors: fields}})
	return false
}

type connectionJSON struct {
	ID        contracts.DatabaseID `json:"id"`
	Name      string               `json:"name"`
	Host      string               `json:"host"`
	Port      int                  `json:"port"`
	Database  string               `json:"database"`
	Username  string               `json:"username"`
	SSLMode   string               `json:"ssl_mode"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

func safeConnection(c database.Connection) connectionJSON {
	return connectionJSON{c.ID, c.Name, c.Host, c.Port, c.Database, c.Username, c.SSLMode, c.CreatedAt, c.UpdatedAt}
}

type databaseJSON struct {
	ID    contracts.DatabaseID  `json:"id"`
	Name  string                `json:"name"`
	Level contracts.AccessLevel `json:"level"`
}

func safeDatabase(c database.DatabaseChoice) databaseJSON { return databaseJSON{c.ID, c.Name, c.Level} }

type columnJSON struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Default  string `json:"default"`
	Nullable bool   `json:"nullable"`
}
type definitionJSON struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

func safeDefinitions(items []database.Definition) []definitionJSON {
	out := make([]definitionJSON, 0, len(items))
	for _, item := range items {
		out = append(out, definitionJSON{item.Name, item.SQL})
	}
	return out
}
func safeDetails(d database.TableDetails) any {
	columns := make([]columnJSON, 0, len(d.Columns))
	for _, c := range d.Columns {
		columns = append(columns, columnJSON{c.Name, c.Type, c.Default, c.Nullable})
	}
	return struct {
		Schema      string           `json:"schema"`
		Name        string           `json:"name"`
		Columns     []columnJSON     `json:"columns"`
		Constraints []definitionJSON `json:"constraints"`
		Indexes     []definitionJSON `json:"indexes"`
	}{d.Schema, d.Name, columns, safeDefinitions(d.Constraints), safeDefinitions(d.Indexes)}
}
func safePage(p database.DataPage) any {
	type cell struct {
		Text      string `json:"text"`
		Null      bool   `json:"null"`
		Truncated bool   `json:"truncated"`
	}
	rows := make([][]cell, 0, len(p.Rows))
	for _, row := range p.Rows {
		items := make([]cell, 0, len(row))
		for _, c := range row {
			items = append(items, cell{c.Text, c.Null, c.Truncated})
		}
		rows = append(rows, items)
	}
	if p.Columns == nil {
		p.Columns = []string{}
	}
	return struct {
		Columns []string `json:"columns"`
		Rows    [][]cell `json:"rows"`
		Page    int      `json:"page"`
		HasNext bool     `json:"has_next"`
	}{p.Columns, rows, p.Page, p.HasNext}
}
func safeDiff(d database.SchemaDiff) any {
	type change struct {
		Kind   string              `json:"kind"`
		Action string              `json:"action"`
		Table  string              `json:"table"`
		Name   string              `json:"name"`
		Before string              `json:"before"`
		After  string              `json:"after"`
		Risk   database.ChangeRisk `json:"risk"`
		SQL    string              `json:"sql"`
	}
	type unsupported struct {
		Schema string `json:"schema"`
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Table  string `json:"table"`
	}
	changes := make([]change, 0, len(d.Changes))
	for _, c := range d.Changes {
		changes = append(changes, change{c.Kind, c.Action, c.Table, c.Name, c.Before, c.After, c.Risk, c.SQL})
	}
	objects := make([]unsupported, 0, len(d.Unsupported))
	for _, o := range d.Unsupported {
		objects = append(objects, unsupported{o.Schema, o.Name, o.Kind, o.Table})
	}
	return struct {
		ReferenceSchema string        `json:"reference_schema"`
		TargetSchema    string        `json:"target_schema"`
		Changes         []change      `json:"changes"`
		Unsupported     []unsupported `json:"unsupported"`
		SQL             string        `json:"sql"`
	}{d.ReferenceSchema, d.TargetSchema, changes, objects, d.SQL}
}

func (h *databaseHandler) serve(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.CurrentUser(r)
	if !ok || actor == "" {
		WriteError(w, 401, "UNAUTHENTICATED", "Sesi Anda berakhir. Silakan masuk kembali.")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if parts[0] != "connections" && parts[0] != "databases" {
		WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	if parts[0] == "connections" {
		if err := h.authorizer.RequirePlatformAdmin(r.Context(), actor); err != nil {
			h.fail(w, err)
			return
		}
	}
	if len(parts) > 1 && !validID(parts[1]) {
		WriteError(w, 400, "INVALID_REQUEST", "ID tidak valid.")
		return
	}
	if parts[0] == "connections" {
		h.connection(w, r, actor, parts)
	} else {
		h.target(w, r, actor, parts)
	}
}

func (h *databaseHandler) connection(w http.ResponseWriter, r *http.Request, actor contracts.UserID, parts []string) {
	if r.URL.RawQuery != "" {
		WriteError(w, 400, "INVALID_REQUEST", "Parameter query tidak didukung.")
		return
	}
	ctx := r.Context()
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			items, err := h.connections.List(ctx, actor)
			if err != nil {
				h.fail(w, err)
				return
			}
			out := make([]connectionJSON, 0, len(items))
			for _, item := range items {
				out = append(out, safeConnection(item))
			}
			writeData(w, struct {
				Connections []connectionJSON `json:"connections"`
			}{out})
		case http.MethodPost:
			var in connectionInput
			if !decodePlatform(w, r, &in) {
				return
			}
			if !validateConnection(w, in, true) {
				return
			}
			item, err := h.connections.Create(ctx, actor, in.value())
			if err != nil {
				h.fail(w, err)
				return
			}
			writeJSON(w, 201, struct {
				Data any `json:"data"`
			}{struct {
				Connection connectionJSON `json:"connection"`
			}{safeConnection(item)}})
		default:
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	if len(parts) != 2 && !(len(parts) == 3 && parts[2] == "test") {
		WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	id := contracts.DatabaseID(parts[1])
	if len(parts) == 3 {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		if err := h.connections.TestConnection(ctx, actor, id); err != nil {
			h.fail(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	switch r.Method {
	case http.MethodGet:
		item, err := h.connections.Get(ctx, actor, id)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeData(w, struct {
			Connection connectionJSON `json:"connection"`
		}{safeConnection(item)})
	case http.MethodPatch:
		var in connectionInput
		if !decodePlatform(w, r, &in) {
			return
		}
		if !validateConnection(w, in, false) {
			return
		}
		item, err := h.connections.Update(ctx, actor, id, in.value())
		if err != nil {
			h.fail(w, err)
			return
		}
		writeData(w, struct {
			Connection connectionJSON `json:"connection"`
		}{safeConnection(item)})
	case http.MethodDelete:
		if err := h.connections.Delete(ctx, actor, id); err != nil {
			h.fail(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		methodNotAllowed(w, "GET, PATCH, DELETE")
	}
}

func queryFields(r *http.Request, names ...string) ([]string, bool) {
	q := r.URL.Query()
	if len(q) != len(names) {
		return nil, false
	}
	values := make([]string, len(names))
	for i, name := range names {
		v := q[name]
		if len(v) != 1 || v[0] == "" {
			return nil, false
		}
		values[i] = v[0]
	}
	return values, true
}

func (h *databaseHandler) target(w http.ResponseWriter, r *http.Request, actor contracts.UserID, parts []string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	ctx := r.Context()
	if len(parts) == 1 {
		if r.URL.RawQuery != "" {
			WriteError(w, 400, "INVALID_REQUEST", "Parameter query tidak didukung.")
			return
		}
		items, err := h.connections.Accessible(ctx, actor)
		if err != nil {
			h.fail(w, err)
			return
		}
		out := make([]databaseJSON, 0, len(items))
		for _, item := range items {
			out = append(out, safeDatabase(item))
		}
		writeData(w, struct {
			Databases []databaseJSON `json:"databases"`
		}{out})
		return
	}
	if len(parts) > 3 {
		WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
		return
	}
	id := contracts.DatabaseID(parts[1])
	var result any
	var err error
	if len(parts) == 2 {
		if r.URL.RawQuery != "" {
			WriteError(w, 400, "INVALID_REQUEST", "Parameter query tidak didukung.")
			return
		}
		var choice database.DatabaseChoice
		choice, err = h.connections.Describe(ctx, actor, id)
		result = struct {
			Database databaseJSON `json:"database"`
		}{safeDatabase(choice)}
	} else {
		var names []string
		switch parts[2] {
		case "schemas":
			names = []string{}
		case "tables":
			names = []string{"schema"}
		case "table":
			names = []string{"schema", "table"}
		case "data":
			names = []string{"schema", "table", "page"}
		case "schema-diff":
			names = []string{"reference_schema", "target_schema"}
		default:
			WriteError(w, 404, "NOT_FOUND", "Endpoint tidak ditemukan.")
			return
		}
		values, valid := queryFields(r, names...)
		if !valid {
			WriteError(w, 400, "INVALID_REQUEST", "Parameter query tidak valid.")
			return
		}
		switch parts[2] {
		case "schemas":
			var items []string
			items, err = h.viewer.Schemas(ctx, actor, id)
			if items == nil {
				items = []string{}
			}
			result = struct {
				Schemas []string `json:"schemas"`
			}{items}
		case "tables":
			var items []database.Table
			items, err = h.viewer.Tables(ctx, actor, id, values[0])
			type table struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			}
			out := make([]table, 0, len(items))
			for _, item := range items {
				out = append(out, table{item.Name, item.Kind})
			}
			result = struct {
				Tables []table `json:"tables"`
			}{out}
		case "table":
			var item database.TableDetails
			item, err = h.viewer.Details(ctx, actor, id, values[0], values[1])
			result = struct {
				Table any `json:"table"`
			}{safeDetails(item)}
		case "data":
			page, e := strconv.Atoi(values[2])
			if e != nil || page < 1 || page > 1000000 {
				WriteError(w, 400, "INVALID_REQUEST", "Halaman tidak valid.")
				return
			}
			var item database.DataPage
			item, err = h.viewer.Data(ctx, actor, id, values[0], values[1], page)
			result = struct {
				Page any `json:"page"`
			}{safePage(item)}
		case "schema-diff":
			if values[0] == values[1] {
				WriteError(w, 400, "INVALID_REQUEST", "Schema harus berbeda.")
				return
			}
			var item database.SchemaDiff
			item, err = h.viewer.Diff(ctx, actor, id, values[0], values[1])
			result = struct {
				Diff any `json:"diff"`
			}{safeDiff(item)}
		}
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	writeData(w, result)
}

func (h *databaseHandler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contracts.ErrForbidden):
		WriteError(w, 403, "FORBIDDEN", "Akses ditolak.")
	case errors.Is(err, database.ErrNotFound):
		WriteError(w, 404, "NOT_FOUND", "Data tidak ditemukan.")
	case errors.Is(err, database.ErrConflict), errors.Is(err, database.ErrInUse):
		WriteError(w, 409, "CONFLICT", "Perubahan bertentangan dengan data saat ini.")
	case errors.Is(err, database.ErrInvalidInput):
		WriteError(w, 422, "VALIDATION_FAILED", "Isian tidak valid.")
	case errors.Is(err, database.ErrConnection):
		WriteError(w, 503, "CONNECTION_UNAVAILABLE", "Koneksi target tidak tersedia.")
	default:
		WriteError(w, 503, "SERVICE_UNAVAILABLE", "Layanan sementara tidak tersedia.")
	}
}
