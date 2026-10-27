package fleet

// macosLockdownScript closes the ways a process of the job user could become admin on a macOS
// guest without root, besides knowing admin's password (which is the VM's own random one, see
// guestpassword.go). It runs on every macOS guest, shared or sealed, before any job can start:
//
//   - su and login authenticate only a caller already in the admin group (pam_group on the
//     requesting user, ruser), so the job user can't su or login to admin even with the password.
//     A caller that is root still passes su through pam_rootok, and sudo, which Orchard and grove
//     use as admin, has its own PAM file and is untouched.
//   - system.privilege.admin, the right behind GUI-style privilege prompts (osascript's "with
//     administrator privileges", AuthorizationExecuteWithPrivileges), is denied outright. sudo
//     doesn't go through authorization rights.
//
// SSH to admin is closed by the pf anchor (macosSSHBlockRule). Every step is idempotent, since the
// script runs at every boot, and a failed one stops the script before Nomad takes work.
const macosLockdownScript = `# --- the job user can't become admin ---
grove_pam_line='auth       required       pam_group.so no_warn group=admin ruser # grove'
for grove_pam in /etc/pam.d/su /etc/pam.d/login; do
  [ -f "$grove_pam" ] || continue
  if ! grep -qxF "$grove_pam_line" "$grove_pam"; then
    grove_pam_new=$(awk -v line="$grove_pam_line" '
      !done && /^auth[[:space:]]/ && !/pam_rootok/ { print line; done = 1 }
      { print }
      END { if (!done) print line }
    ' "$grove_pam")
    printf '%s\n' "$grove_pam_new" | grove_priv tee "$grove_pam.grove" >/dev/null
    grove_priv chmod 0444 "$grove_pam.grove"
    grove_priv mv -f "$grove_pam.grove" "$grove_pam"
  fi
  if ! grep -qxF "$grove_pam_line" "$grove_pam"; then
    echo "grove: could not gate $grove_pam to the admin group" >&2
    exit 1
  fi
done
if ! grove_priv security authorizationdb write system.privilege.admin deny >/dev/null 2>&1; then
  echo "grove: could not deny the system.privilege.admin right" >&2
  exit 1
fi
`
