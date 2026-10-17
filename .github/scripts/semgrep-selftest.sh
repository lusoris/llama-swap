#!/usr/bin/env bash
# Plants the cases each HISS-08 rule must report, and the cases it must leave
# alone, in a temporary folder and runs `semgrep scan --test` on them, so the
# scan is proven to bite. The plants are written here rather than committed
# because the repository's own scans would report them. Run it from the
# repository root.
set -euo pipefail

rules=.config/semgrep/hiss
plant="$(mktemp -d)"
trap 'rm -r "$plant"' EXIT
cp "$rules"/*.yml "$plant/"

# Assembled at run time so the planted calls do not appear in this file.
e="ev""al"
sh="s""h"

# Each fixture shares its rule file's name; `ruleid:` marks a line the rule
# must report, `ok:` a line it must not.
printf '%s\n' \
  'package hiss' '' 'import (' '	"context"' '	"os/exec"' ')' '' \
  'func shell(ctx context.Context, s string) error {' \
  '	// ruleid: hiss-08-go-shell-command' \
  "	if err := exec.Command(\"$sh\", \"-c\", s).Run(); err != nil {" \
  '		return err' '	}' \
  '	// ruleid: hiss-08-go-shell-command' \
  "	if err := exec.CommandContext(ctx, \"/bin/ba$sh\", \"-c\", s).Run(); err != nil {" \
  '		return err' '	}' \
  '	// ok: hiss-08-go-shell-command' \
  '	return exec.CommandContext(ctx, "echo", "-c", s).Run()' '}' \
  > "$plant/hiss-08-go.go"

printf '%s\n' \
  'export function dynamic(s: string): unknown {' \
  '  // ruleid: hiss-08-ts-string-timer' \
  '  setTimeout("tick()", 10);' \
  '  // ok: hiss-08-ts-string-timer' \
  '  setTimeout(() => s, 10);' \
  '  // ruleid: hiss-08-ts-eval' \
  '  const f = new Function("a", s);' \
  '  // ok: hiss-08-ts-eval' \
  '  const data = JSON.parse(s);' \
  '  // ruleid: hiss-08-ts-eval' \
  "  return $e(s) ?? f ?? data;" '}' \
  > "$plant/hiss-08-typescript.ts"

printf '%s\n' \
  '<script lang="ts">' \
  "  let { src }: { src: string } = \$props();" \
  '  // ruleid: hiss-08-svelte-eval' \
  '  const run = new Function(src);' \
  '  // ruleid: hiss-08-svelte-eval' \
  '  setTimeout("run()", 5);' \
  '  // ruleid: hiss-08-svelte-eval' \
  "  const value = $e(src);" \
  '  // ok: hiss-08-svelte-eval' \
  '  const ok = evaluate(src);' \
  '</script>' '' '<p>{run}{value}{ok}</p>' \
  > "$plant/hiss-08-svelte.svelte"

printf '%s\n' \
  '#!/usr/bin/env bash' "cmd=\"\$1\"" \
  '# ruleid: hiss-08-shell-eval' \
  "$e \"\$cmd\"" \
  '# ok: hiss-08-shell-eval' \
  "echo \"running $e with \$cmd\"" \
  '# ruleid: hiss-08-shell-eval' \
  "if true; then $e \"\$cmd\"; fi" \
  '# ruleid: hiss-08-shell-eval' \
  "x=\$($e \"\$cmd\")" \
  '# ruleid: hiss-08-shell-eval' \
  "echo \"\$x\" && $e \"\$cmd\"" \
  > "$plant/hiss-08-shell.sh"

# semgrep skips a fixture without a rule file of the same name, and a rule
# file without a fixture, so each must have its partner.
unpaired=0
for f in "$plant"/*; do
  base="${f%.*}"
  if [[ "$f" == *.yml ]]; then
    partners=("$base".*)
    [[ "${#partners[@]}" -ge 2 ]] || { echo "no fixture for ${f##*/}" >&2; unpaired=1; }
  elif [[ ! -f "$base.yml" ]]; then
    echo "no rule file for fixture ${f##*/}" >&2
    unpaired=1
  fi
done
[[ "$unpaired" -eq 0 ]] || exit 1

semgrep scan --test --metrics=off --disable-version-check "$plant"
