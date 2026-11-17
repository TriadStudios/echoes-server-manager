#!/bin/sh
# Run by the service unit of "esm service install" before every start of ESM
# (ExecStartPre), with the path of the ESM binary as its only argument.
#
# A self-update marks the update as not confirmed yet: <binary>.update.pending.
# The new ESM removes that file once it has been up for 30 seconds. While the
# file is there, this script counts the starts, one line each in
# <binary>.update.starts. At the 3rd start the new ESM has failed to stay up
# twice, and the script puts the old binary back instead of starting the new
# one a third time:
#
#   <binary>                     moved aside as <binary>.failed
#   <binary>.previous            moved back as <binary>
#   <binary>.update.rolledback   written: the pending record and the time
#   <binary>.update.pending and <binary>.update.starts   removed
#
# ESM reads the rolledback file when it starts, writes the audit entry and
# shows the rollback on GET /api/v1/box and on the Box page.
#
# If the old binary is the binary that is there already (the update was killed
# before the new one went in place), the mark is stale and is dropped without
# putting anything back.
#
# The new binary may not run at all, so this uses nothing of it: /bin/sh and
# the usual small tools (date, ln, cp, mv, rm, sed, cmp). Plain POSIX sh.
#
# It never stops ESM from starting. With no pending file it does nothing.
# Whatever else goes wrong, it says so on standard output (the service log)
# and ends with status 0.

# The start at which the old binary is put back (release.RollbackStarts).
starts_allowed=3

exe=$1
[ -n "$exe" ] || exit 0
pending=$exe.update.pending
starts=$exe.update.starts
previous=$exe.previous
failed=$exe.failed
rolledback=$exe.update.rolledback

say() {
	printf 'esm update rollback: %s\n' "$*"
}

# No update waits for its confirmation: a normal start.
[ -f "$pending" ] || exit 0

info=
IFS= read -r info < "$pending"

# A stale mark: an update that was killed after it kept the old binary and
# marked itself, but before the new binary went in place, leaves $previous as
# the binary that is there already: the same file, or a copy with the same
# bytes. There is nothing to put back, a file cannot replace itself, and the
# mark would outlive a binary put there by hand later and roll it back to the
# old one. So the mark goes at once, whatever the start count. (-ef is no
# POSIX test, but dash and busybox have it; a shell that does not answers
# with status 2, and cmp decides.)
if [ -f "$previous" ]; then
	if [ "$previous" -ef "$exe" ] 2>/dev/null || cmp -s "$previous" "$exe" 2>/dev/null; then
		say "the update $info never got its new binary in place: $previous is $exe itself; the mark is dropped"
		rm -f "$pending" "$starts"
		exit 0
	fi
fi

now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
if ! printf '%s\n' "$now" >> "$starts"; then
	say "cannot write $starts; this update is not watched"
	exit 0
fi
n=0
while IFS= read -r _line; do
	n=$((n + 1))
done < "$starts"

if [ "$n" -lt "$starts_allowed" ]; then
	say "the update $info is not confirmed yet; this is start $n, the old binary is put back at start $starts_allowed"
	exit 0
fi

if [ ! -f "$previous" ]; then
	say "start $n of the update $info, which was not confirmed, but $previous is missing: there is nothing to put back, the new binary stays"
	rm -f "$pending" "$starts"
	exit 0
fi

say "start $n of the update $info, which was not confirmed: putting $previous back as $exe (the new binary is kept as $failed)"
# The failed binary is kept: a second name for the same file, or a copy when
# the file system has no links. Then the old binary is moved over the new one
# in one step, so that $exe is never missing.
rm -f "$failed"
if ! ln "$exe" "$failed" 2>/dev/null && ! cp -p "$exe" "$failed" 2>/dev/null; then
	say "cannot keep $exe as $failed; going on without it"
fi
if ! mv -f "$previous" "$exe"; then
	say "cannot move $previous over $exe; the new binary stays and the next start tries again"
	exit 0
fi

# The record: the pending record with the time added. Written to a file of its
# own and moved in place, so ESM never reads half of it.
tmp=$rolledback.tmp
if sed -e 's/[[:space:]]*}[[:space:]]*$/,"rolled_back_at":"'"$now"'"}/' "$pending" > "$tmp" 2>/dev/null &&
	[ -s "$tmp" ] && mv -f "$tmp" "$rolledback"; then
	:
else
	printf '{"rolled_back_at":"%s"}\n' "$now" > "$rolledback" 2>/dev/null || say "cannot write $rolledback"
fi
rm -f "$tmp" "$pending" "$starts"
say "the old binary is back at $exe"
exit 0
