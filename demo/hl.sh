#!/usr/bin/env bash
# hl.sh — live-demo highlighter. Reads stdin, prints EVERY line unchanged
# except the given substrings, which are emphasized (bold white on green) so
# the audience's eye lands on the proof. Authentic output, just spotlighted.
#
#   Usage:  <command> | ./hl.sh "0% packet loss" "PEERED"
#           <command> | ./hl.sh -r 'HTTP 2[0-9][0-9]'   # -r = args are regex
#
# Colour default: bold white on green (matches the playbook's ✅ tone).
# Override e.g. HL_SGR='1;97;41' (red) for the "isolated / withdrawn" beats.
REGEX=0
[ "${1:-}" = "-r" ] && { REGEX=1; shift; }
[ $# -eq 0 ] && exec cat
HL_SGR="${HL_SGR:-1;97;42}" HL_REGEX="$REGEX" exec perl -pe '
  BEGIN { @pats=@ARGV; @ARGV=(); $|=1; $sgr=$ENV{HL_SGR}; $rgx=$ENV{HL_REGEX} }
  for my $p (@pats) {
    my $rx = $rgx ? $p : quotemeta($p);
    s/($rx)/\e[${sgr}m$1\e[0m/g;
  }
' "$@"
