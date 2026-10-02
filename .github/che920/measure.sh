#!/usr/bin/env bash
# CHE-920 hosted-runner quiet-host measurement. Instrumentation only: the candidate tree is untouched.
# usage: measure.sh baseline | run     env: BIN (dir with n.norace.test, n.race.test), OUT, N, CAP, WIN
set -u
N=${N:-100}; CAP=${CAP:-1200}; WIN=${WIN:-10}; OUT=${OUT:-che920-out}
# Quiet criteria, fixed before the run. The issue's quiet reference is ~2.5 load on a 56-CPU host = ~4.5% per CPU.
MAX_BUSY=5.0      # % non-idle, steal excluded, all vCPUs, over the guard window
MAX_STEAL=2.0     # % steal (hypervisor/neighbour contention)
MAX_PSI_MEM=1.0   # memory PSI "some" avg10
mkdir -p "$OUT/raw"

stat_line() { awk '/^cpu /{t=$2+$3+$4+$5+$6+$7+$8+$9; print t, $5+$6, $9}' /proc/stat; }
swap_ctr()  { awk '/^pswpin /{i=$2} /^pswpout /{o=$2} END{print i+0, o+0}' /proc/vmstat; }
psi()       { if [ -r "/proc/pressure/$1" ]; then awk '/^some/{split($2,a,"="); print a[2]}' "/proc/pressure/$1"; else echo na; fi; }

guard() { # label -> one TSV line, exit 0 only if QUIET
  local label=$1 a b w0 w1 pc pm pio l1
  a=($(stat_line)); w0=($(swap_ctr)); sleep "$WIN"; b=($(stat_line)); w1=($(swap_ctr))
  pc=$(psi cpu); pm=$(psi memory); pio=$(psi io); l1=$(cut -d' ' -f1 /proc/loadavg)
  awk -v l="$label" -v dt=$((b[0]-a[0])) -v di=$((b[1]-a[1])) -v ds=$((b[2]-a[2])) \
      -v swi=$((w1[0]-w0[0])) -v swo=$((w1[1]-w0[1])) -v pc="$pc" -v pm="$pm" -v pio="$pio" -v l1="$l1" \
      -v mb=$MAX_BUSY -v ms=$MAX_STEAL -v mp=$MAX_PSI_MEM 'BEGIN{
        busy=100*(dt-di-ds)/dt; steal=100*ds/dt;
        ok=(busy<=mb && steal<=ms && swi==0 && swo==0 && (pm=="na" || pm+0<=mp));
        printf "%s\tbusy_excl_steal=%.2f\tsteal=%.2f\tswpin=%d\tswpout=%d\tpsi_cpu10=%s\tpsi_mem10=%s\tpsi_io10=%s\tload1=%s\t%s\n", l,busy,steal,swi,swo,pc,pm,pio,l1,(ok?"QUIET":"NOT-QUIET");
        exit ok?0:1 }' | tee -a "$OUT/guards.tsv"
  return "${PIPESTATUS[0]}"
}

sampler() { local a b; a=($(stat_line)); while sleep 1; do b=($(stat_line))
  awk -v e="$(date +%s)" -v dt=$((b[0]-a[0])) -v di=$((b[1]-a[1])) -v ds=$((b[2]-a[2])) -v pm="$(psi memory)" -v pc="$(psi cpu)" -v l1="$(cut -d' ' -f1 /proc/loadavg)" -v sw="$(swap_ctr | tr ' ' '+')" \
    'BEGIN{ if(dt<=0)exit; printf "%s\t%.2f\t%.2f\t%s\t%s\t%s\t%s\n", e,100*(dt-di-ds)/dt,100*ds/dt,pc,pm,l1,sw }' >> "$OUT/samples.tsv"
  a=("${b[@]}"); done; }

case "${1:-}" in
baseline)
  { echo "runner: ${RUNNER_NAME:-?} image=${ImageVersion:-?} os=$(. /etc/os-release; echo "$PRETTY_NAME") kernel=$(uname -r)"
    echo "cpus=$(nproc) model=$(awk -F: '/model name/{print $2; exit}' /proc/cpuinfo) virt=$(systemd-detect-virt 2>/dev/null || echo ?)"
    free -m | sed -n 1,3p; df -h / | tail -1; cat /proc/loadavg; } | tee "$OUT/env.txt"
  guard baseline_after_checkout; exit 0 ;;
run) ;;
*) echo "usage: $0 baseline|run" >&2; exit 2 ;;
esac

sampler & SP=$!; trap 'kill $SP 2>/dev/null' EXIT
sleep 30   # our own build/migrate activity just ended; one settle, no retries afterwards
T0=$(date +%s); : > "$OUT/runs.tsv"; : > "$OUT/batches.tsv"
batch() { # mode test
  local mode=$1 name=$2 tag="$1_$2" bin="$BIN/n.$1.test" pass=0 fail=0 skip=0 notrun=0 pre post bs be i t ms res rc v
  guard "pre_$tag"; pre=$?
  bs=$(date +%s)
  for i in $(seq 1 "$N"); do
    if [ $(( $(date +%s) - T0 )) -ge "$CAP" ]; then notrun=$((N-i+1)); break; fi
    t=$(date +%s%N)
    res=$("$bin" -test.v -test.run "^${name}\$" -test.count=1 -test.timeout=120s 2>&1); rc=$?
    ms=$(( ($(date +%s%N)-t)/1000000 ))
    if [ $rc -eq 0 ] && grep -q -- "--- PASS: ${name} " <<<"$res"; then v=PASS; pass=$((pass+1))
    elif [ $rc -eq 0 ] && grep -q -- "--- SKIP" <<<"$res"; then v=SKIP; skip=$((skip+1)); printf '%s\n' "$res" > "$OUT/raw/${tag}_run$i.txt"
    else v=FAIL; fail=$((fail+1)); printf '%s\n' "$res" > "$OUT/raw/${tag}_run$i.txt"; fi
    printf '%s\t%s\t%s\t%s\t%s\n' "$tag" "$i" "$rc" "$v" "$ms" >> "$OUT/runs.tsv"
  done
  be=$(date +%s); guard "post_$tag"; post=$?
  local st; st=$(awk -F'\t' -v s="$bs" -v e="$be" '$1>=s && $1<=e{n++; ms+=$3; if($3>mx)mx=$3; if($5+0>mp)mp=$5+0; split($7,w,"+"); if(!f){w0=w[1]+w[2]; f=1} w1=w[1]+w[2]} END{printf "samples=%d mean_steal=%.2f max_steal=%.2f max_psi_mem10=%.2f swap_delta=%d", n, (n?ms/n:0), mx, mp, w1-w0}' "$OUT/samples.tsv")
  printf '%s\tn=%d\tpass=%d\tfail=%d\tskip=%d\tnotrun=%d\tpre=%s\tpost=%s\t%s\tstart=%s\tend=%s\n' "$tag" "$N" "$pass" "$fail" "$skip" "$notrun" "$([ $pre -eq 0 ] && echo QUIET || echo NOT-QUIET)" "$([ $post -eq 0 ] && echo QUIET || echo NOT-QUIET)" "$st" "$bs" "$be" | tee -a "$OUT/batches.tsv"
}
for mode in norace race; do
  for name in TestObserve_EditAfterSettledCreateIsAdmittedAgainstRealBudgetService TestObserve_SecondEditWithoutRevisionChangeReusesReservation; do batch "$mode" "$name"; done
done
echo "measurement wall=$(( $(date +%s) - T0 ))s cap=${CAP}s" | tee "$OUT/wall.txt"
{ echo "## CHE-920 hosted-runner measurement"; echo '```'; cat "$OUT/env.txt" 2>/dev/null; echo; cat "$OUT/guards.tsv"; echo; cat "$OUT/batches.tsv"; cat "$OUT/wall.txt"; echo '```'; } >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
exit 0
