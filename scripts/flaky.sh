#!/bin/sh
# flaky.sh <test files>: runs the Test functions that the given Go test files
# add or change since BASE (default HEAD~1, the previous commit) twenty times
# under the race detector, one go test per package, and fails naming each test
# that failed and the run it first failed on (#265).
set -u

base=${BASE:-HEAD~1}
count=20
list=$(mktemp)
trap 'rm -f "$list"' EXIT

# tests prints the names of the Test functions of a Go test file.
tests() {
	sed -n 's/^func \(Test[A-Za-z0-9_]*\)([A-Za-z0-9_]* \*testing\.T).*/\1/p' "$1"
}

for file in "$@"; do
	case $file in *_test.go) ;; *) continue ;; esac
	[ -f "$file" ] || continue
	file=${file#./}
	dir=$(dirname "$file")
	if git cat-file -e "$base:./$file" 2>/dev/null; then
		continue
	fi
	for name in $(tests "$file"); do
		printf '%s %s\n' "./${dir#./}" "$name" >>"$list"
	done
done

if [ ! -s "$list" ]; then
	echo "flaky: no new or changed tests"
	exit 0
fi

status=0
for pkg in $(cut -d' ' -f1 "$list" | sort -u); do
	names=$(awk -v pkg="$pkg" '$1 == pkg { print $2 }' "$list" | sort -u | paste -sd '|' -)
	echo "flaky: go test -race -count=$count -run '^($names)\$' $pkg"
	out=$(go test -race -count="$count" -v -run "^($names)\$" "$pkg" 2>&1)
	rc=$?
	[ "$rc" -eq 0 ] && continue
	status=1
	# Counts the runs of each top-level test and names its first failing run;
	# the lines of passing runs are left out.
	printf '%s\n' "$out" | awk -v pkg="$pkg" -v count="$count" '
		/^=== RUN / && $3 !~ /\// { runs[$3]++; next }
		/^=== (RUN|PAUSE|CONT|NAME) / || /^ *--- PASS: / || /^PASS$/ { next }
		/^--- FAIL: / {
			if (!($3 in first)) { first[$3] = runs[$3]; order[++n] = $3 }
			failed[$3]++
		}
		{ print }
		END {
			for (i = 1; i <= n; i++)
				printf "flaky: %s failed on run %d of %d in %s (%d of %d runs failed)\n", order[i], first[order[i]], count, pkg, failed[order[i]], count
			if (n == 0) printf "flaky: go test failed in %s\n", pkg
		}'
done
exit "$status"
