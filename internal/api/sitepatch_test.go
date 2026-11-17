package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

// Every field a change call (PATCH /instances/{name}) takes is classified
// here, for the site role: those it may change and those it may not. The
// test fails when the change request gets a field that is in neither list,
// so a new field is a decision made on purpose; sitePatchFields in site.go,
// which the code goes by, has to say the same.
var (
	siteMayChange = []string{"display_name", "max_players", "listed", "rules", "auto_update", "update_max_wait", "autostart",
		"restart_on_crash", "schedule", "backup", "map", "world", "cluster", "channel"}
	siteMayNotChange = []string{"ports", "bind_ip", "limits", "extra_args", "secret_delivery", "build"}
)

// jsonFields lists the JSON names of the fields of a struct, those of an
// embedded struct included.
func jsonFields(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var out []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, jsonFields(t, f.Type)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Errorf("%s.%s has no JSON name: give it one, so that it can be said what the site role may do with it", typ.Name(), f.Name)
			continue
		}
		out = append(out, name)
	}
	return out
}

func TestEveryChangeFieldIsClassifiedForTheSiteRole(t *testing.T) {
	fields := jsonFields(t, reflect.TypeOf(instances.PatchRequest{}))
	if len(fields) < 15 {
		t.Fatalf("only %d fields found in the change request: %v", len(fields), fields)
	}
	for _, f := range fields {
		may, mayNot := slices.Contains(siteMayChange, f), slices.Contains(siteMayNotChange, f)
		switch {
		case may && mayNot:
			t.Errorf("%q is in both lists of the test", f)
		case !may && !mayNot:
			t.Errorf("the change request has a field %q that is not classified: decide whether the site role may change it, "+
				"put it in siteMayChange or siteMayNotChange here and, if it may, in sitePatchFields in site.go", f)
		case sitePatchFields[f] != may:
			t.Errorf("%q: sitePatchFields says %v, the test says %v", f, sitePatchFields[f], may)
		}
	}
	for _, f := range slices.Concat(siteMayChange, siteMayNotChange) {
		if !slices.Contains(fields, f) {
			t.Errorf("the test names %q, which the change request has no more", f)
		}
	}
	for f := range sitePatchFields {
		if !slices.Contains(fields, f) {
			t.Errorf("sitePatchFields has %q, which the change request has no more", f)
		}
	}
	if len(sitePatchFields) != len(siteMayChange) {
		t.Errorf("sitePatchFields has %d fields, the test %d", len(sitePatchFields), len(siteMayChange))
	}
}

// A site token that names a field it may not change gets 403 naming it,
// whatever the value, and nothing is changed: not the fields it may change
// either, sent beside it. This ESM has no instances, so a call that got
// past the check would fail, which is how the test knows that the refusal
// came first.
func TestSiteTokenIsRefusedTheFieldsItMayNotChange(t *testing.T) {
	sr := newSiteRig(t)
	patch := func(body string, more map[string]string) (rec *httptest.ResponseRecorder) {
		defer func() {
			if recover() != nil {
				t.Errorf("site token, %s: the call went on to the instance", body)
				rec = nil
			}
		}()
		return sr.asRole(auth.RoleSite, "PATCH", "/api/v1/instances/alpha", body, more)
	}
	for _, c := range []struct{ body, first string }{
		{`{"extra_args":["-Foo"]}`, "extra_args"},
		{`{"limits":{"memory_max":"8G","cpu_quota_percent":100}}`, "limits"},
		{`{"bind_ip":"192.0.2.5"}`, "bind_ip"},
		{`{"ports":{"game":30000,"query":31000}}`, "ports"},
		{`{"secret_delivery":"command_line"}`, "secret_delivery"},
		{`{"build":"1.1"}`, "build"},
		{`{"build":""}`, "build"},
		{`{"extra_args":null}`, "extra_args"},
		{`{"extra_args":[]}`, "extra_args"},
		{`{"EXTRA_ARGS":["-x"]}`, "EXTRA_ARGS"},
		{`{"Extra_Args":null}`, "Extra_Args"},
		{`{"extra\u005fargs":["-x"]}`, "extra_args"},
		{`{"display_name":"Alpha","extra_args":["-x"]}`, "extra_args"},
		{`{"extra_args":["-x"],"display_name":"Alpha","max_players":5}`, "extra_args"},
		{`{"max_players":5,"max_players":6,"limits":null}`, "limits"},
		{`{"rules":{"A":"b"},"bind_ip":"","ports":null}`, "bind_ip, ports"},
		{`{"a_field_added_later":1}`, "a_field_added_later"},
		{`{"":1}`, ";"}, // an empty name: the refusal still says what it may change
		{`{"DISPLAY_NAME":"x"}`, "DISPLAY_NAME"},
		{`{"display_name ":"x"}`, "display_name "},
	} {
		before := sr.auditCount()
		rec := patch(c.body, header(ActingUserHeader, "jane"))
		if rec == nil {
			continue
		}
		if rec.Code != http.StatusForbidden || errorCode(rec) != "forbidden" ||
			!strings.Contains(rec.Body.String(), "the site role may not change "+c.first) ||
			!strings.Contains(rec.Body.String(), "it may change auto_update, autostart, backup, channel, cluster, display_name, listed, map, max_players,") {
			t.Errorf("site token, %s: %d %s", c.body, rec.Code, rec.Body)
			continue
		}
		e := sr.lastAudit()
		if sr.auditCount() != before+1 || e.OK || e.Action != "instance.change" || e.Target != "alpha" || e.Actor.Role != "site" ||
			e.Actor.ActingUser != "jane" || !strings.Contains(e.Error, "may not change") {
			t.Errorf("site token, %s: audit entry %+v", c.body, e)
		}
	}
	// Many names are cut short, and so is a long one.
	rec := patch(`{"a":1,"b":1,"c":1,"d":1,"e":1,"f":1,"g":1,"`+strings.Repeat("x", 300)+`":1}`, nil)
	if rec == nil || rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "may not change a, b, c, d, e, ...;") ||
		strings.Contains(rec.Body.String(), strings.Repeat("x", 41)) {
		t.Errorf("many fields: %d %s", rec.Code, rec.Body)
	}
	// The other roles are told what they always were: this needs admin.
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleRead} {
		rec := sr.asRole(role, "PATCH", "/api/v1/instances/alpha", `{"display_name":"x"}`, nil)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "needs the admin role") {
			t.Errorf("%s token: %d %s", role, rec.Code, rec.Body)
		}
	}
}

// limitPatch lets through what the site role may send, in any combination
// and with any value, and leaves what it cannot read to decode (400); an
// admin is not limited at all; and the body can be read again afterwards.
func TestLimitPatchLooksAtTheNamesOfTheFields(t *testing.T) {
	check := func(role auth.Role, body string) error {
		t.Helper()
		r := httptest.NewRequest("PATCH", "/api/v1/instances/alpha", strings.NewReader(body))
		err := (&call{role: role}).limitPatch(r)
		if b, rerr := io.ReadAll(r.Body); rerr != nil || string(b) != body {
			t.Errorf("the body was %q after the check (read: %v), it was %q", b, rerr, body)
		}
		return err
	}
	for _, f := range siteMayChange {
		for _, v := range []string{`null`, `"x"`, `1`, `{"a":"b"}`, `["-x"]`} {
			if err := check(auth.RoleSite, `{"`+f+`":`+v+`}`); err != nil {
				t.Errorf("site role, field %s: %v", f, err)
			}
		}
	}
	all := `{"` + strings.Join(siteMayChange, `":1,"`) + `":1}`
	for _, body := range []string{all, `{}`, ``, ` `, `null`, `[]`, `"x"`, `7`, `{`, `{"display_name":`, `not json`} {
		if err := check(auth.RoleSite, body); err != nil {
			t.Errorf("site role, body %q: %v", body, err)
		}
	}
	for _, f := range siteMayNotChange {
		err := check(auth.RoleSite, `{"`+f+`":null}`)
		var ae *apiError
		if !errors.As(err, &ae) || ae.status != http.StatusForbidden || !strings.Contains(err.Error(), "may not change "+f) {
			t.Errorf("site role, field %s: %v", f, err)
		}
		if err := check(auth.RoleAdmin, `{"`+f+`":null}`); err != nil {
			t.Errorf("admin role, field %s: %v", f, err)
		}
	}
}
