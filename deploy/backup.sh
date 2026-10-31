#!/bin/sh
# Scheduled pg_dump for deploy/compose.yaml's backup service.
#
# Postgres holds what cannot be rebuilt from raw mail (§4.2, §7): prefix
# labels, rules, audit, principals and credentials, drafts. This takes a
# full logical dump every BACKUP_INTERVAL seconds and keeps the newest
# BACKUP_KEEP of them in /backups.
#
# Why a shell loop and not cron or a backup image: it runs in the same
# pinned postgres image as the server, so pg_dump always matches it; it
# adds no daemon and no third-party image; and the whole policy is this
# file. Getting the dumps off this machine is up to the operator (README).
#
#   yuuka-backup         run forever: dump, prune, sleep until the next slot
#   yuuka-backup once    one dump and prune, then exit
#   yuuka-backup check   exit 0 if a dump newer than 2 intervals exists
#
# Connection settings come from PGHOST, PGUSER, PGDATABASE, PGPASSWORD.
set -eu

dir=/backups
interval=${BACKUP_INTERVAL:-86400}
keep=${BACKUP_KEEP:-14}

case $interval in '' | *[!0-9]* | 0) echo "BACKUP_INTERVAL must be a positive number of seconds" >&2; exit 2 ;; esac
case $keep in '' | *[!0-9]* | 0) echo "BACKUP_KEEP must be a positive number" >&2; exit 2 ;; esac

# Dumps contain credential hashes and token ciphertexts (§13.3).
umask 077

dump() {
	name="yuuka-$(date -u +%Y%m%dT%H%M%SZ).dump"
	tmp="$dir/.partial-$name"
	# Custom format: compressed, and pg_restore can pick objects from it.
	# A dump only gets its final name once pg_restore can read it back, so
	# a half-written file is never mistaken for a backup or counted by the
	# pruning below.
	if pg_dump --format=custom --file="$tmp" && pg_restore --list "$tmp" >/dev/null; then
		mv "$tmp" "$dir/$name"
		echo "backup: wrote $name ($(du -h "$dir/$name" | cut -f1))"
		prune
	else
		rm -f "$tmp"
		echo "backup: FAILED; previous dumps left untouched" >&2
		return 1
	fi
}

# Keep the newest $keep dumps, counted rather than aged: if dumps start
# failing, the old ones stay instead of expiring one by one. Names sort by
# time, and contain no spaces.
prune() {
	for f in "$dir"/yuuka-*.dump; do
		[ -e "$f" ] && echo "$f"
	done | sort -r | tail -n +"$((keep + 1))" |
		while read -r old; do
			rm -f "$old" && echo "backup: pruned ${old##*/}"
		done
}

case ${1:-loop} in
check)
	# find -mmin wants minutes; at least one.
	[ -n "$(find "$dir" -maxdepth 1 -name 'yuuka-*.dump' -mmin -"$((interval * 2 / 60 + 1))")" ]
	;;
once)
	dump
	;;
loop)
	echo "backup: every ${interval}s, keeping $keep dumps in $dir"
	rm -f "$dir"/.partial-* # left by a container stopped mid-dump
	while :; do
		dump || true # logged; the health check turns unhealthy if it persists
		# Sleep to the next multiple of the interval since the epoch (UTC),
		# so a daily backup runs at 00:00 UTC rather than whenever the
		# container last started.
		sleep "$((interval - $(date +%s) % interval))"
	done
	;;
*)
	echo "usage: yuuka-backup [loop|once|check]" >&2
	exit 2
	;;
esac
