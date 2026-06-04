package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

type adminIndexData struct {
	base
}

type adminUsersData struct {
	base
	Users []UserInfo
}

// handleAdminIndex renders GET /admin.
func (s *server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	d := adminIndexData{
		base: base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
	}
	if err := s.renderBuffered(w, "admin.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "admin_index", http.StatusOK)
}

// handleAdminUsers renders GET /admin/users.
func (s *server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.renderError(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sess := SessionFromContext(r.Context())
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.logger.Error("admin users: list", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	d := adminUsersData{
		base:  base{Session: sess, CSRF: issueCSRF(w, requestIsTLS(r, s.trustProxy)), Flash: takeFlash(w, r)},
		Users: users,
	}
	if err := s.renderBuffered(w, "admin_users.html", d); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "render error")
		return
	}
	EmitRequestMetric(r.Context(), s.logger, "admin_users", http.StatusOK)
}

// handleAdminUserCreate processes POST /admin/users/create.
func (s *server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const base = "/admin/users"
	fail := func(msg string) {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "create", "invalid")
		s.redirectFlash(w, r, base, msg)
	}
	name := r.PostFormValue("name")
	if name == "" {
		fail("name required")
		return
	}
	password := r.PostFormValue("password")
	if password != "" && len(password) < 8 {
		fail("password too short (min 8)")
		return
	}
	isAdmin := r.PostFormValue("is_admin") == "on"

	_, err := s.store.CreateUser(r.Context(), name, isAdmin)
	if err != nil {
		if errors.Is(err, auth.ErrConflict) {
			fail("user already exists")
			return
		}
		s.logger.Error("admin: create user", "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "create", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	passwordSet := false
	if password != "" {
		if err := s.store.SetPassword(r.Context(), name, password); err != nil {
			s.logger.Error("admin: set password after create", "user", name, "err", err)
			EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "create", "error")
			s.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		passwordSet = true
	}

	s.emitAdmin(r.Context(), "auth.user.created",
		slog.String("user", name),
		slog.Bool("is_admin", isAdmin),
		slog.Bool("password_set", passwordSet),
	)
	EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "create", "ok")
	s.redirectFlash(w, r, base, "user created: "+name)
}

// handleAdminUserDisable processes POST /admin/users/disable.
func (s *server) handleAdminUserDisable(w http.ResponseWriter, r *http.Request) {
	s.handleAdminUserSetDisabled(w, r, true)
}

// handleAdminUserEnable processes POST /admin/users/enable.
func (s *server) handleAdminUserEnable(w http.ResponseWriter, r *http.Request) {
	s.handleAdminUserSetDisabled(w, r, false)
}

func (s *server) handleAdminUserSetDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	if !s.requireAdmin(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const base = "/admin/users"
	name := r.PostFormValue("name")
	sess := SessionFromContext(r.Context())

	if disabled && sess != nil && name == sess.Name {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "disable", "invalid")
		s.redirectFlash(w, r, base, "cannot disable yourself")
		return
	}

	if err := s.store.SetUserDisabled(r.Context(), name, disabled); err != nil {
		if errors.Is(err, sqlitestore.ErrLastAdmin) {
			EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "disable", "invalid")
			s.redirectFlash(w, r, base, err.Error())
			return
		}
		s.logger.Error("admin: set user disabled", "user", name, "disabled", disabled, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "disable", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	if disabled {
		s.emitAdmin(r.Context(), "auth.user.disabled", slog.String("user", name))
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "disable", "ok")
		s.redirectFlash(w, r, base, "user disabled: "+name)
	} else {
		s.emitAdmin(r.Context(), "auth.user.enabled", slog.String("user", name))
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "enable", "ok")
		s.redirectFlash(w, r, base, "user enabled: "+name)
	}
}

// handleAdminUserDelete processes POST /admin/users/delete.
func (s *server) handleAdminUserDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const base = "/admin/users"
	name := r.PostFormValue("name")
	confirm := r.PostFormValue("confirm")
	sess := SessionFromContext(r.Context())
	fail := func(msg string) {
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "delete", "invalid")
		s.redirectFlash(w, r, base, msg)
	}
	if confirm != name {
		fail("confirmation does not match username")
		return
	}
	if sess != nil && name == sess.Name {
		fail("cannot delete yourself")
		return
	}
	if err := s.store.DeleteUser(r.Context(), name); err != nil {
		if errors.Is(err, sqlitestore.ErrLastAdmin) || errors.Is(err, sqlitestore.ErrReservedUser) || errors.Is(err, auth.ErrNoSuchUser) {
			fail(err.Error())
			return
		}
		s.logger.Error("admin: delete user", "user", name, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "delete", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.user.deleted", slog.String("user", name))
	EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "delete", "ok")
	s.redirectFlash(w, r, base, "user deleted: "+name)
}

// handleAdminUserEmail processes POST /admin/users/email.
func (s *server) handleAdminUserEmail(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if !s.postGuard(w, r) {
		return
	}
	const base = "/admin/users"
	name := r.PostFormValue("name")
	email := r.PostFormValue("email")
	if err := s.store.SetEmail(r.Context(), name, email); err != nil {
		if errors.Is(err, auth.ErrConflict) {
			EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "set_email", "invalid")
			s.redirectFlash(w, r, base, "email already in use")
			return
		}
		if errors.Is(err, auth.ErrNoSuchUser) {
			EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "set_email", "invalid")
			s.redirectFlash(w, r, base, "user not found")
			return
		}
		s.logger.Error("admin: set email", "user", name, "err", err)
		EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "set_email", "error")
		s.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	s.emitAdmin(r.Context(), "auth.user.email_set",
		slog.String("user", name),
		slog.String("email", email),
	)
	EmitAdminActionMetric(r.Context(), s.logger, "admin_users", "set_email", "ok")
	s.redirectFlash(w, r, base, "email updated")
}
