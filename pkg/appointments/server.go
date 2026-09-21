package appointments

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"time"
)

type Server struct {
	store *Store
	tz    *time.Location
}

func NewServer(store *Store, tz *time.Location) *Server {
	return &Server{store: store, tz: tz}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/appointments", s.handleAPI)
	return mux
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return srv.ListenAndServe()
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "db down", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	today := time.Now().In(s.tz).Format("2006-01-02")
	records, err := s.store.LiveToday(r.Context(), today)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	if err := indexTmpl.Execute(w, pageData{Date: today, Records: records}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().In(s.tz).Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", date, s.tz); err != nil {
		http.Error(w, "invalid date", http.StatusBadRequest)
		return
	}
	records, err := s.store.LiveToday(r.Context(), date)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	type card struct {
		PatientName string `json:"patient_name"`
		HipName     string `json:"hip_name"`
		TokenNumber string `json:"token_number"`
		ConfirmedAt string `json:"confirmed_at"`
	}
	out := make([]card, 0, len(records))
	for _, r := range records {
		out = append(out, card{
			PatientName: r.PatientName,
			HipName:     r.HipName,
			TokenNumber: r.TokenNumber,
			ConfirmedAt: r.ConfirmedAtUTC.Format(time.RFC3339),
		})
	}
	fmt.Fprintf(w, `{"date":%q,"cards":[`+"\n", date)
	for i, c := range out {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, `{"patient_name":%q,"hip_name":%q,"token_number":%q,"confirmed_at":%q}`,
			c.PatientName, c.HipName, c.TokenNumber, c.ConfirmedAt)
	}
	fmt.Fprint(w, "]}\n")
}

type pageData struct {
	Date    string
	Records []Record
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="30">
<title>AIIMS Live Appointments — {{.Date}}</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 720px; margin: 2rem auto; padding: 0 1rem; color: #1a1a1a; }
  h1 { font-size: 1.4rem; margin-bottom: 0.25rem; }
  .date { color: #666; font-size: 0.9rem; margin-bottom: 1.5rem; }
  .card { border: 1px solid #ddd; border-radius: 8px; padding: 1rem; margin-bottom: 0.75rem; background: #fff; box-shadow: 0 1px 2px rgba(0,0,0,0.04); }
  .card .live { display: inline-block; background: #16a34a; color: white; padding: 2px 8px; border-radius: 4px; font-size: 0.75rem; font-weight: 600; letter-spacing: 0.05em; }
  .card h2 { font-size: 1.1rem; margin: 0.5rem 0 0.25rem; }
  .card .meta { font-size: 0.85rem; color: #555; }
  .card .token { font-family: ui-monospace, SFMono-Regular, monospace; font-size: 1.5rem; font-weight: 600; color: #16a34a; margin: 0.5rem 0; }
  .empty { color: #666; padding: 2rem; text-align: center; border: 1px dashed #ddd; border-radius: 8px; }
</style>
</head>
<body>
<h1>AIIMS Live Appointments</h1>
<div class="date">{{.Date}} · auto-refresh 30s</div>
{{if .Records}}
  {{range .Records}}
  <div class="card">
    <span class="live">LIVE</span>
    <h2>{{.PatientName}}</h2>
    <div class="token">{{.TokenNumber}}</div>
    <div class="meta">{{.HipName}} · confirmed {{.ConfirmedAtUTC.Format "15:04:05 UTC"}}</div>
  </div>
  {{end}}
{{else}}
  <div class="empty">No live appointments for {{.Date}}.</div>
{{end}}
</body>
</html>
`))
