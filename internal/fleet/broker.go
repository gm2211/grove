package fleet

import (
	"fmt"
	"strings"

	"github.com/gm2211/grove/internal/broker"
)

// brokerScript renders the part of a shared pool's startup script that finds the worker's
// credential broker (internal/broker) and tells the VM's Nomad client about it. The broker listens
// on the VM gateway, broker.Port, the one host address the guest fences let jobs reach. The script
// fetches its CA from there (as root, which the fences don't cover), checks it is a CA whose name
// constraints permit only the broker's inject hosts (a subset: an older guest refuses a newer
// broker's extra host, the safe way round) with every IP address excluded, and advertises
// the broker's URL and the CA as node meta (grove-broker.hcl, beside grove-meta.hcl). nomad/jobs'
// run.sh points the job's HTTP(S) proxy at that URL and adds the CA to the bundle it trusts.
//
// The fetch is plain HTTP, but only the host answers on the gateway, and the check means even a
// forged CA could vouch for no site the real one can't. A CA that fails the check, or no broker
// within two minutes, leaves the VM without one: jobs still run, and a job holding placeholders
// is told why its credentials don't work.
func brokerScript() string {
	return strings.NewReplacer(
		"@HOSTS@", strings.Join(broker.InjectHosts(), " "),
		"@PORT@", fmt.Sprint(broker.Port),
		"@TRIES@", fmt.Sprint(brokerFetchTries),
	).Replace(brokerScriptTemplate)
}

// brokerFetchTries is how many times, five seconds apart, a booting VM asks its gateway for the
// broker's CA: two minutes, for a worker whose broker starts after its VMs. Tests that run a whole
// startup script against the real network set it to 1.
var brokerFetchTries = 24

const brokerScriptTemplate = `# --- credential broker ---
# Jobs reach the worker's credential broker on the VM gateway, and nothing else on the host.
if [ "$(uname -s)" = "Darwin" ]; then
  grove_broker_meta=/usr/local/etc/nomad.d/grove-broker.hcl
  grove_gateway=$(route -n get default 2>/dev/null | awk '$1 == "gateway:" { print $2; exit }')
else
  grove_broker_meta=/etc/nomad.d/grove-broker.hcl
  grove_gateway=$(ip -4 route show default 2>/dev/null | awk '$1 == "default" && $2 == "via" { print $3; exit }')
fi
case "$grove_gateway" in
  "" | *[!0-9.]*) grove_gateway="" ;;
esac
grove_priv rm -f "$grove_broker_meta"

# The broker's CA must be one certificate, a CA that can sign only leaves, whose critical name
# constraints permit nothing but hosts this script knows the broker serves (any of them, in any
# order) and exclude every IP address.
grove_broker_ca_ok() {
  [ "$(grep -c '^-----BEGIN ' "$1")" = 1 ] || return 1
  [ "$(sed -n 1p "$1")" = "-----BEGIN CERTIFICATE-----" ] || return 1
  [ "$(sed -n '$p' "$1")" = "-----END CERTIFICATE-----" ] || return 1
  grove_ca_text=$(openssl x509 -in "$1" -noout -text 2>/dev/null) || return 1
  printf '%s\n' "$grove_ca_text" | grep -Eq '^ *CA:TRUE, pathlen:0$' || return 1
  grove_ca_constraints=$(printf '%s\n' "$grove_ca_text" | awk '
    !inside && /X509v3 Name Constraints: critical$/ { inside = 1; match($0, /^ */); indent = RLENGTH; next }
    inside { match($0, /^ */); if (RLENGTH <= indent) exit; gsub(/^ +| +$/, ""); print }
  ' | sed -e 's|^IP:0\.0\.0\.0/0\.0\.0\.0$|IP:0.0.0.0/0|' -e 's|^IP:[0:]*:[0:]*/[0:]*$|IP:::/0|')
  grove_ca_section=""
  grove_ca_permitted=0
  grove_ca_excluded=""
  while IFS= read -r grove_ca_line; do
    case "$grove_ca_line" in
      Permitted:) grove_ca_section=permitted ;;
      Excluded:) grove_ca_section=excluded ;;
      DNS:*)
        [ "$grove_ca_section" = permitted ] || return 1
        case " @HOSTS@ " in
          *" ${grove_ca_line#DNS:} "*) grove_ca_permitted=$((grove_ca_permitted + 1)) ;;
          *) return 1 ;;
        esac
        ;;
      IP:0.0.0.0/0 | IP:::/0)
        [ "$grove_ca_section" = excluded ] || return 1
        grove_ca_excluded="$grove_ca_excluded $grove_ca_line"
        ;;
      *) return 1 ;;
    esac
  done <<GROVE_CA_CONSTRAINTS
$grove_ca_constraints
GROVE_CA_CONSTRAINTS
  [ "$grove_ca_permitted" -gt 0 ] || return 1
  case "$grove_ca_excluded" in *" IP:0.0.0.0/0"*) ;; *) return 1 ;; esac
  case "$grove_ca_excluded" in *" IP:::/0"*) ;; *) return 1 ;; esac
}

grove_broker_ca=""
grove_broker_dir=$(mktemp -d)
if [ -n "$grove_gateway" ]; then
  grove_tries=0
  while [ "$grove_tries" -lt @TRIES@ ]; do
    if curl -fsS --noproxy '*' --max-time 5 --max-filesize 16384 -o "$grove_broker_dir/ca.pem" "http://$grove_gateway:@PORT@/ca.pem" 2>/dev/null; then
      break
    fi
    rm -f "$grove_broker_dir/ca.pem"
    grove_tries=$((grove_tries + 1))
    sleep 5
  done
fi
if [ -s "$grove_broker_dir/ca.pem" ]; then
  if grove_broker_ca_ok "$grove_broker_dir/ca.pem"; then
    grove_broker_ca=$(sed -e '1d' -e '$d' "$grove_broker_dir/ca.pem" | tr -d '\r\n')
    case "$grove_broker_ca" in
      "" | *[!A-Za-z0-9+/=]*) grove_broker_ca="" ;;
    esac
  else
    echo "grove: the CA served on $grove_gateway:@PORT@ is not a grove credential broker's; this VM gets no broker" >&2
  fi
fi
rm -rf "$grove_broker_dir"
if [ -n "$grove_broker_ca" ]; then
  grove_priv tee "$grove_broker_meta" >/dev/null <<GROVE_BROKER_META
client {
  meta {
    grove_broker    = "http://$grove_gateway:@PORT@"
    grove_broker_ca = "$grove_broker_ca"
  }
}
GROVE_BROKER_META
else
  echo "grove: no credential broker on this VM's gateway; jobs here get no brokered credentials" >&2
fi
`
