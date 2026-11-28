#!/bin/sh
# Usage: coverage-summary.sh <go coverage profile> [ui lcov file]
#
# Prints a Markdown table of statement coverage per Go package and the
# total, plus the UI line coverage when an lcov file is given. A profile
# written with -coverpkg holds each block once per test binary: a block
# counts once, and as covered when any binary ran it, like `go tool cover`.
set -eu

profile=$1
lcov=${2:-}

awk '
	NR == 1 { next } # mode line
	{
		split($1, loc, ":")
		file = loc[1]
		pkg = file
		sub(/\/[^\/]*$/, "", pkg)
		if (!($1 in stmts)) {
			stmts[$1] = $2
			pkgof[$1] = pkg
		}
		if ($3 > 0) hit[$1] = 1
	}
	END {
		for (b in stmts) {
			p = pkgof[b]
			total[p] += stmts[b]
			all += stmts[b]
			if (b in hit) {
				covered[p] += stmts[b]
				allCovered += stmts[b]
			}
		}
		print "| Go package | Statements | Covered | Coverage |"
		print "| --- | ---: | ---: | ---: |"
		n = 0
		for (p in total) names[++n] = p
		for (i = 2; i <= n; i++) { # insertion sort: awk has no portable sort
			v = names[i]
			for (j = i - 1; j > 0 && names[j] > v; j--) names[j + 1] = names[j]
			names[j + 1] = v
		}
		for (i = 1; i <= n; i++) {
			p = names[i]
			printf "| `%s` | %d | %d | %.1f%% |\n", p, total[p], covered[p], 100 * covered[p] / total[p]
		}
		printf "| **Total** | **%d** | **%d** | **%.1f%%** |\n", all, allCovered, 100 * allCovered / all
	}
' "$profile"

if [ -n "$lcov" ] && [ -f "$lcov" ]; then
	awk -F: '
		$1 == "LF" { found += $2 }
		$1 == "LH" { hit += $2 }
		END {
			if (found > 0) printf "\nUI (`ui/bundle.js`) line coverage: %.1f%% (%d of %d lines)\n", 100 * hit / found, hit, found
		}
	' "$lcov"
fi
