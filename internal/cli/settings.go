package cli

import (
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
)

// gameSettings is "esm game-settings [--group G]": the catalog of the
// game's settings, a line for each, by group. It is the one verb besides
// logs and help that prints more than one line; with --json it prints the
// API's object.
func (e *env) gameSettings(args []string) error {
	fs := e.flags("game-settings")
	group := fs.String("group", "", "only the settings of this group (access, world, chat or reports)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("game-settings takes no arguments")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var cat api.GameSettingList
	if err := c.do(e.ctx, "GET", "/api/v1/game-settings", nil, &cat); err != nil {
		return err
	}
	if *group != "" {
		var ids []string
		for _, g := range cat.Groups {
			ids = append(ids, g.ID)
		}
		if !slices.Contains(ids, *group) {
			return usageErr("--group must be one of %s", strings.Join(ids, ", "))
		}
		cat.Groups = slices.DeleteFunc(cat.Groups, func(g gameserver.SettingGroup) bool { return g.ID != *group })
		cat.Settings = slices.DeleteFunc(cat.Settings, func(s gameserver.GameSetting) bool { return s.Group != *group })
	}
	if e.g.json {
		return e.result(cat, "")
	}
	// One table, so every column lines up; a group's title is a row of
	// its own.
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tTYPE\tDEFAULT\tALLOWED\tNOTES")
	for _, g := range cat.Groups {
		fmt.Fprintf(tw, "[%s]\t\t\t\t\n", g.Title)
		for _, s := range cat.Settings {
			if s.Group == g.ID {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Key, s.Type, orDash(s.Default), orDash(allowedWords(s)), notes(s))
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
		if _, err := fmt.Fprintln(e.out, strings.TrimRight(line, " ")); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(e.out, "Set one with create --set Key=Value or on the settings page; a secret with esm secret. World rules take effect only when a world is first created.")
	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// allowedWords says briefly what a setting allows.
func allowedWords(s gameserver.GameSetting) string {
	f := func(p *float64) string { return strconv.FormatFloat(*p, 'f', -1, 64) }
	var out string
	switch s.Type {
	case gameserver.TypeInt, gameserver.TypeFloat:
		switch {
		case s.Min != nil && s.Max != nil:
			out = f(s.Min) + ".." + f(s.Max)
		case s.Min != nil:
			out = f(s.Min) + ".."
		}
		if s.Unit != "" {
			out += " " + s.Unit
		}
	case gameserver.TypeEnum:
		var values []string
		for _, c := range s.Choices {
			values = append(values, c.Value)
		}
		out = strings.Join(values, "|")
	case gameserver.TypeBool:
		out = "True|False"
	case gameserver.TypeDate:
		out = "YYYY-MM-DD, not in the future"
	case gameserver.TypeList:
		out = "comma list, " + strconv.Itoa(s.MaxLength) + " max"
	case gameserver.TypeString:
		if s.Secret {
			break
		}
		out = strconv.Itoa(s.MaxLength) + " max"
		if s.Format == gameserver.FormatURL {
			out = "http(s) address"
		}
	}
	return out
}

// notes are the flags of a setting and its label.
func notes(s gameserver.GameSetting) string {
	var out []string
	if s.NewWorldOnly {
		out = append(out, "new world only")
	}
	if s.Caution {
		out = append(out, "caution")
	}
	if s.Secret {
		out = append(out, "secret: esm secret <name> "+strings.ReplaceAll(s.SecretField, "_", "-"))
	}
	return strings.Join(append(out, s.Label), "; ")
}

// secrets are the passwords and tokens esm secret sets: the argument that
// names one, its field in the secrets call, and its name in words.
var secrets = []struct{ arg, field, words string }{
	{"server-password", "server_password", "server password"},
	{"admin-password", "admin_password", "admin password"},
	{"announcements-token", "announcements_token", "announcements token"},
	{"problem-report-token", "problem_report_token", "problem report token"},
}

// secret is "esm secret <name> <which> [--clear]": set a password or token
// of an instance, read from standard input (never from the command line,
// where other users could see it), or remove it.
func (e *env) secret(args []string, stdin io.Reader) error {
	fs := e.flags("secret")
	clear := fs.Bool("clear", false, "remove it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	var names []string
	for _, s := range secrets {
		names = append(names, s.arg)
	}
	if len(pos) != 2 {
		return usageErr("secret needs an instance name and one of %s", strings.Join(names, ", "))
	}
	i := slices.IndexFunc(secrets, func(s struct{ arg, field, words string }) bool { return s.arg == pos[1] })
	if i < 0 {
		// The argument is not repeated: it may be a value typed in the
		// wrong place.
		return usageErr("the second argument of secret must be one of %s", strings.Join(names, ", "))
	}
	which := secrets[i]
	value := ""
	if !*clear {
		if value, err = readSecretLine(stdin, "esm secret <name> "+which.arg); err != nil {
			return err
		}
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var out api.SecretsResult
	if err := c.do(e.ctx, "PUT", "/api/v1/instances/"+url.PathEscape(pos[0])+"/secrets", map[string]string{which.field: value}, &out); err != nil {
		return err
	}
	line := "set the " + which.words + " of " + pos[0]
	if *clear {
		line = "removed the " + which.words + " of " + pos[0]
	}
	if out.RestartRequired {
		line += "; the running server gets it at its next start"
	}
	return e.result(out, warn(line, out.Warnings, out.UnsafeStoredSettings))
}
