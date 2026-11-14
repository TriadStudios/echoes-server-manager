package instances

import (
	"fmt"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
)

// An instance whose secret_delivery is "command_line" hands its passwords
// to the server on the command line. The server prints its command line
// into its log and its output, and its failures may repeat it. ESM does not
// try to clean that up for the read role: while an instance is set this
// way, the read role sees none of its server output and none of its
// failure details. Operators and admins see both, masked as far as the
// output filter goes, with no promise that nothing gets through.

// HiddenNotice stands in for what the read role may not see of an instance
// that passes its passwords on the server command line: its state reason,
// and its server output.
const HiddenNotice = "hidden from the read role: this instance passes its passwords on the server command line (secret_delivery \"command_line\"), so its server output and failure details may show them"

// ErrOutputHidden refuses the server output of such an instance to the read
// role.
var ErrOutputHidden = fault.New(fault.Forbidden, "hidden_unsafe_secrets", HiddenNotice)

// OutputHidden reports whether the read role is kept from the instance's
// server output and failure details.
func (in Instance) OutputHidden() bool {
	return in.SecretDelivery == gameserver.DeliveryCommandLine
}

// ForRead returns the status as the read role sees it when the instance's
// output is hidden: with HiddenNotice in place of the reason, whatever the
// reason was.
func (s Status) ForRead() Status {
	s.Reason = HiddenNotice
	return s
}

// ForRead returns the view as the read role sees it.
func (v View) ForRead() View {
	if v.OutputHidden() {
		v.Status = v.Status.ForRead()
		if v.Update.LastError != "" {
			v.Update.LastError = HiddenNotice
		}
	}
	return v
}

// Warnings an instance view can carry.
const (
	// WarnSecretsOnCommandLine: the instance passes its passwords on the
	// server command line. Unsafe on a shared box.
	WarnSecretsOnCommandLine = "secrets_on_command_line"
	// WarnLimitsNotEnforced: the instance has resource limits, and the
	// supervisor in use does not apply them.
	WarnLimitsNotEnforced = "limits_not_enforced"
	// WarnSecretsUnconfirmed: the instance has passwords or tokens and hands them
	// over in the secrets file, and its running server has not said, in its
	// status file, that it took them. A server build that cannot read the
	// secrets file runs without them; this is how that shows.
	WarnSecretsUnconfirmed = "secrets_unconfirmed"
	// WarnMaxPlayersAboveGame: the instance was stored with a max_players
	// above what the game server takes (gameserver.MaxPlayersLimit); the
	// server uses its limit.
	WarnMaxPlayersAboveGame = "max_players_above_game_limit"
	// WarnUnsafeStoredSettings: settings stored by an earlier version break
	// the rules for values on the server command line; the view names them
	// in UnsafeStoredSettings. One with an en dash keeps the server from
	// starting (see enDashIn).
	WarnUnsafeStoredSettings = "unsafe_stored_settings"
	// WarnCommandLineTooLong: the server command line of the instance, as
	// an earlier version stored its settings, is longer than a change may
	// make it (gameserver.MaxLineLength). Beyond what ESM starts a server
	// with (gameserver.StartLineLimit) ESM does not start it.
	WarnCommandLineTooLong = "command_line_too_long"
)

// WarningText says in words what a warning of an instance view means.
func WarningText(code string) string {
	switch code {
	case WarnSecretsOnCommandLine:
		return "UNSAFE on a shared box: passwords and tokens are on the server command line (secret_delivery command_line), where every account on this box can read them from the process list, and the server writes them into its log"
	case WarnLimitsNotEnforced:
		return "memory and CPU limits are not enforced by the process supervisor"
	case WarnSecretsUnconfirmed:
		return "the running server has not confirmed that it took its passwords and tokens from the secrets file; a server build that cannot read that file runs WITHOUT passwords and tokens"
	case WarnMaxPlayersAboveGame:
		return fmt.Sprintf("max_players is above %d, the most the game server takes; the server uses %d, and a change of max_players must be at most %d",
			gameserver.MaxPlayersLimit, gameserver.MaxPlayersLimit, gameserver.MaxPlayersLimit)
	case WarnUnsafeStoredSettings:
		return "settings stored by an earlier version of ESM break the rules for values on the server command line, so the server may read them otherwise than ESM shows: an empty one is left out when the server starts, any other is passed as it is stored, but with an en dash (U+2013) on its command line the server's engine does not start, and ESM does not start it then; change or remove them, or, for the name of an instance, make the instance anew under another name"
	case WarnCommandLineTooLong:
		return fmt.Sprintf("the server command line of this instance is longer than %d characters, the most a change may lead to; the server's engine does not start with more than %d, and ESM, which keeps %d of them free for what a build's start script may add, does not start a server whose line is longer than %d: shorten or remove rules or extra arguments",
			gameserver.MaxLineLength, gameserver.EngineLineLimit, gameserver.StartRoom, gameserver.StartLineLimit)
	}
	return code
}

// WarningWords says a warning of an instance view in words, with the
// settings it is about: unsafeStored, the view's UnsafeStoredSettings, for
// unsafe_stored_settings.
func WarningWords(code string, unsafeStored []string) string {
	text := WarningText(code)
	if code == WarnUnsafeStoredSettings && len(unsafeStored) > 0 {
		text += " (" + strings.Join(unsafeStored, ", ") + ")"
	}
	return text
}

// unsafeSuffix is added to the name of an output file when it is set
// aside.
const unsafeSuffix = ".unsafe"

// setAsideOutput moves the files that hold the output of an instance's
// server (the console output, the server log and the one before it) out
// of the way, to the same name with unsafeSuffix, replacing what was set
// aside before. ESM never shows the files set aside. It is done when an
// instance stops passing its passwords on the command line: what its
// server wrote until then holds them, and from then on the read role may
// see the output again.
func (m *Manager) setAsideOutput(name string) error {
	for _, rel := range []string{consoleFile(name), serverLog(name), previousServerLog(name)} {
		if !m.st.Exists(rel) {
			continue
		}
		if err := m.st.Rename(rel, rel+unsafeSuffix); err != nil {
			return err
		}
	}
	return nil
}
