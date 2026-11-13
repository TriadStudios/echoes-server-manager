package api

import (
	"errors"
	"net/http"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
	"github.com/TriadStudios/echoes-server-manager/internal/steam"
)

// --- ESM's own updates ---

// SelfUpdateRequest is the body of POST /box/self-update.
type SelfUpdateRequest struct {
	// Force takes the newest release even when it is not newer (or this
	// ESM is a development build).
	Force bool `json:"force"`
}

// SelfUpdateResult answers POST /box/self-update.
type SelfUpdateResult struct {
	release.Result
	// Restarting says that the service ends in a moment, for its service
	// manager to start the new binary.
	Restarting bool   `json:"restarting"`
	Note       string `json:"note"`
}

func (s *Server) selfUpdate(w http.ResponseWriter, r *http.Request, c *call) error {
	var req SelfUpdateRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	c.target = "esm"
	if s.o.SelfUpdate == nil {
		return errStatus(http.StatusConflict, "not_available", "this ESM cannot update itself")
	}
	res, err := s.o.SelfUpdate(r.Context(), req.Force)
	switch {
	case errors.Is(err, release.ErrUpToDate):
		return errStatus(http.StatusConflict, "up_to_date", "%v", err)
	case errors.Is(err, release.ErrUnknownVersion):
		return errStatus(http.StatusConflict, "unknown_version", "%v", err)
	case errors.Is(err, release.ErrBusy):
		return errStatus(http.StatusConflict, "busy", "%v", err)
	case err != nil:
		return fault.Wrap(fault.Failed, "update_failed", err, "self-update failed")
	}
	c.target = "esm " + res.From + " to " + res.To
	return writeJSON(w, http.StatusOK, SelfUpdateResult{Result: res, Restarting: true,
		Note: "ESM ends in a moment so that its service manager starts the new binary (the unit of \"esm service install\" does); game servers keep running. Without a service manager, start it again by hand."})
}

// --- Steam sources ---

// SteamList answers GET /steam.
type SteamList struct {
	Sources []steam.SourceView `json:"sources"`
}

// SteamPasswordRequest is the body of PUT /steam/{name}/password.
type SteamPasswordRequest struct {
	// Password is the branch password; "" removes it.
	Password string `json:"password"`
}

// SteamPasswordResult answers PUT /steam/{name}/password. The password is
// never sent back.
type SteamPasswordResult struct {
	Name        string `json:"name"`
	PasswordSet bool   `json:"password_set"`
}

func (s *Server) listSteam(w http.ResponseWriter, r *http.Request, c *call) error {
	out := SteamList{Sources: []steam.SourceView{}}
	if s.o.Steam != nil {
		out.Sources = s.o.Steam.Sources()
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) steamPassword(w http.ResponseWriter, r *http.Request, c *call) error {
	name := r.PathValue("name")
	c.target = name
	var req SteamPasswordRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if s.o.Steam == nil {
		return fault.NotFoundf("no Steam source %q", name)
	}
	if err := s.o.Steam.SetPassword(name, req.Password); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, SteamPasswordResult{Name: name, PasswordSet: req.Password != ""})
}

// installSteam runs a Steam source: POST /builds {"source": "steam:<name>"}.
// A build id that is installed already answers 200 with that build, a new
// one 201.
func (s *Server) installSteam(w http.ResponseWriter, r *http.Request, c *call, name string, req InstallRequest) error {
	if req.SHA256 != "" {
		return fault.Invalidf("sha256 does not apply to a Steam source")
	}
	if s.o.Steam == nil {
		return fault.New(fault.Invalid, "no_source", "no Steam source %q", name)
	}
	b, installed, err := s.o.Steam.Install(r.Context(), name, true)
	if err != nil {
		return err
	}
	c.target = steam.SourcePrefix + name + " " + b.Version
	status := http.StatusOK
	if installed {
		status = http.StatusCreated
	}
	return writeJSON(w, status, s.buildView(b, s.o.Instances.BuildUsers()))
}
