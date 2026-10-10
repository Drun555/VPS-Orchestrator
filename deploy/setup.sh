#!/usr/bin/env bash

set -Eeuo pipefail

readonly INSTALL_DIR="/opt/vps-reality"
readonly RUNTIME_DIR="${INSTALL_DIR}/runtime"
readonly XRAY_CONFIG="/usr/local/etc/xray/config.json"
readonly NGINX_SITE="/etc/nginx/sites-available/vps-reality"
readonly NGINX_SITE_LINK="/etc/nginx/sites-enabled/vps-reality"
readonly SSH_OVERRIDE="/etc/ssh/sshd_config.d/00-vps-reality.conf"
readonly XRAY_INSTALLER_URL="https://github.com/XTLS/Xray-install/raw/main/install-release.sh"

SITE_TEMPLATE="file-hosting"
INSTALLATION_ID=""
CURRENT_STAGE="initialization"
SSH_PUBLIC_KEY=""
DUCKDNS_TOKEN=""
DUCKDNS_DOMAIN=""
CERTBOT_EMAIL=""
REALITY_PRIVATE_KEY=""
REALITY_PUBLIC_KEY=""
REALITY_SHORT_ID=""

log() {
  printf '[vps-reality] %s\n' "$*"
}

warn() {
  printf '[vps-reality] WARNING: %s\n' "$*" >&2
}

die() {
  printf '[vps-reality] ERROR: %s\n' "$*" >&2
  exit 1
}

on_error() {
  local exit_code=$?
  local line_number=${1:-unknown}
  printf '[vps-reality] ERROR: stage "%s" failed at line %s (exit %s).\n' \
    "$CURRENT_STAGE" "$line_number" "$exit_code" >&2
  exit "$exit_code"
}

trap 'on_error "$LINENO"' ERR

usage() {
  cat <<'EOF'
Usage:
  bash setup.sh --installation-id INSTANCE_ID --setup-ssh-key 'ssh-ed25519 ...' \
    --duckdns-token TOKEN --duckdns-url example.duckdns.org \
    --certbot-email admin@example.org

Supported operating systems:
  Ubuntu 22.04 LTS, Ubuntu 24.04 LTS, and Ubuntu 26.04 LTS.

Required arguments:
  --site-template  file-hosting (default), syncthing, backup-vault, or minecraft-worlds.
  --installation-id Persistent identifier provided by the orchestrator.
  --setup-ssh-key   Literal OpenSSH public key string. File paths are not read.
  --duckdns-token   DuckDNS account token used by Certbot DNS hooks.
  --duckdns-url     DuckDNS hostname, without a scheme or path.
  --certbot-email   Email address for the Let's Encrypt account.
EOF
}

require_argument_value() {
  local option=$1
  local value=${2-}
  [[ -n "$value" ]] || die "${option} requires a non-empty value."
}

parse_arguments() {
  while (($# > 0)); do
    case "$1" in
      --site-template)
        require_argument_value "$1" "${2-}"
        SITE_TEMPLATE=$2
        shift 2
        ;;
      --installation-id)
        require_argument_value "$1" "${2-}"
        INSTALLATION_ID=$2
        shift 2
        ;;
      --setup-ssh-key)
        require_argument_value "$1" "${2-}"
        SSH_PUBLIC_KEY=$2
        shift 2
        ;;
      --setup-ssh-key=*)
        SSH_PUBLIC_KEY=${1#*=}
        require_argument_value "--setup-ssh-key" "$SSH_PUBLIC_KEY"
        shift
        ;;
      --duckdns-token)
        require_argument_value "$1" "${2-}"
        DUCKDNS_TOKEN=$2
        shift 2
        ;;
      --duckdns-token=*)
        DUCKDNS_TOKEN=${1#*=}
        require_argument_value "--duckdns-token" "$DUCKDNS_TOKEN"
        shift
        ;;
      --duckdns-url)
        require_argument_value "$1" "${2-}"
        DUCKDNS_DOMAIN=$2
        shift 2
        ;;
      --duckdns-url=*)
        DUCKDNS_DOMAIN=${1#*=}
        require_argument_value "--duckdns-url" "$DUCKDNS_DOMAIN"
        shift
        ;;
      --certbot-email)
        require_argument_value "$1" "${2-}"
        CERTBOT_EMAIL=$2
        shift 2
        ;;
      --certbot-email=*)
        CERTBOT_EMAIL=${1#*=}
        require_argument_value "--certbot-email" "$CERTBOT_EMAIL"
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "Unknown argument: $1"
        ;;
    esac
  done

  case "$SITE_TEMPLATE" in
    file-hosting|syncthing|backup-vault|minecraft-worlds) ;;
    *) die "Unknown site template." ;;
  esac
  [[ "$INSTALLATION_ID" =~ ^[a-zA-Z0-9-]+$ ]] || die "--installation-id is required (letters, digits, hyphens)."
  [[ -n "$SSH_PUBLIC_KEY" ]] || die "--setup-ssh-key is required."
  [[ -n "$DUCKDNS_TOKEN" ]] || die "--duckdns-token is required."
  [[ -n "$DUCKDNS_DOMAIN" ]] || die "--duckdns-url is required."
  [[ -n "$CERTBOT_EMAIL" ]] || die "--certbot-email is required."

  DUCKDNS_DOMAIN=${DUCKDNS_DOMAIN,,}
  DUCKDNS_DOMAIN=${DUCKDNS_DOMAIN%.}

  [[ "$DUCKDNS_DOMAIN" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] \
    || die "--duckdns-url must be a hostname without a scheme, port, or path."
  [[ "$DUCKDNS_DOMAIN" == *.* ]] \
    || die "--duckdns-url must contain a fully qualified hostname."
  [[ "$CERTBOT_EMAIL" == *@*.* ]] \
    || die "--certbot-email does not look like an email address."
}

check_environment() {
  [[ ${EUID:-$(id -u)} -eq 0 ]] || die "Run setup.sh as root."
  [[ -r /etc/os-release ]] || die "Cannot identify the operating system."

  # shellcheck disable=SC1091
  source /etc/os-release
  [[ ${ID:-} == "ubuntu" ]] \
    || die "Only Ubuntu 22.04, 24.04, and 26.04 are supported."
  case ${VERSION_ID:-} in
    22.04|24.04|26.04) ;;
    *) die "Only Ubuntu 22.04, 24.04, and 26.04 are supported." ;;
  esac

  command -v flock >/dev/null 2>&1 || die "flock is required."
  exec 9>/run/vps-orchestrator-setup.lock
  flock -n 9 || die "Another installation is running. Retry after it completes."
  if [[ -e "$INSTALL_DIR" ]]; then
    [[ -r "${RUNTIME_DIR}/setup-owner" && $(cat "${RUNTIME_DIR}/setup-owner") == "$INSTALLATION_ID" ]] \
      || die "Existing installation belongs to another setup; refusing to overwrite it."
    [[ ! -f "${RUNTIME_DIR}/domain" || $(cat "${RUNTIME_DIR}/domain") == "$DUCKDNS_DOMAIN" ]] \
      || die "The saved installation has a different domain."
  else
    # Build the ownership marker before atomically claiming the installation directory.
    local claim
    claim=$(mktemp -d /opt/.vps-reality.XXXXXX)
    install -d -m 0700 "$claim/runtime"
    printf '%s\n' "$INSTALLATION_ID" >"$claim/runtime/setup-owner"
    chmod 0755 "$claim"
    mv -T "$claim" "$INSTALL_DIR"
  fi
  install -d -m 0700 "${RUNTIME_DIR}/setup-stages"
}

install_packages() {
  CURRENT_STAGE="package installation"
  log "Installing required Ubuntu packages..."
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y curl nginx jq certbot ufw openssl unzip
}

install_assets() {
  CURRENT_STAGE="asset installation"
  local source_dir
  source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
  install -d -m 0755 "$INSTALL_DIR" "${INSTALL_DIR}/hooks"
  local site_source
  case "$SITE_TEMPLATE" in
    file-hosting) site_source="${source_dir}/index.html" ;;
    syncthing|backup-vault|minecraft-worlds) site_source="${source_dir}/templates/${SITE_TEMPLATE}.html" ;;
    *) die "Unknown site template." ;;
  esac
  install -m 0644 "$site_source" "${INSTALL_DIR}/index.html.next"
  mv -f "${INSTALL_DIR}/index.html.next" "${INSTALL_DIR}/index.html"
  install -m 0700 "${source_dir}/uninstall.sh" "${INSTALL_DIR}/uninstall.sh"
  install -m 0700 "${source_dir}/hooks/duckdns-auth.sh" "${INSTALL_DIR}/hooks/duckdns-auth.sh"
  install -m 0700 "${source_dir}/hooks/duckdns-cleanup.sh" "${INSTALL_DIR}/hooks/duckdns-cleanup.sh"
}

prepare_runtime() {
  CURRENT_STAGE="runtime initialization"
  install -d -m 0700 -o root -g root "$RUNTIME_DIR"
  umask 077
  printf '%s' "$DUCKDNS_TOKEN" >"${RUNTIME_DIR}/duckdns-token"
  printf '%s\n' "$DUCKDNS_DOMAIN" >"${RUNTIME_DIR}/domain"
}

install_xray() {
  CURRENT_STAGE="Xray installation"
  log "Installing Xray Core with the official installer..."

  local installer
  installer=$(mktemp)
  curl -fsSL "$XRAY_INSTALLER_URL" -o "$installer"
  bash "$installer" install
  rm -f "$installer"

  systemctl stop xray.service >/dev/null 2>&1 || true
  [[ -x /usr/local/bin/xray ]] \
    || die "The official Xray installer did not create /usr/local/bin/xray."
}

generate_reality_keys() {
  CURRENT_STAGE="REALITY key generation"
  if [[ -s "${RUNTIME_DIR}/reality-keys.json" ]]; then
    log "Reusing saved REALITY keys."
    load_reality_keys
    return
  fi
  log "Generating REALITY key pair and short ID..."
  local key_output
  key_output=$(/usr/local/bin/xray x25519)
  REALITY_PRIVATE_KEY=$(printf '%s\n' "$key_output" \
    | awk -F': *' 'tolower($1) ~ /^private ?key$/ {print $2; exit}')
  REALITY_PUBLIC_KEY=$(printf '%s\n' "$key_output" \
    | awk -F': *' 'tolower($1) ~ /^(password( \(publickey\))?|public ?key)$/ {print $2; exit}')
  REALITY_SHORT_ID=$(openssl rand -hex 8)

  [[ -n "$REALITY_PRIVATE_KEY" && -n "$REALITY_PUBLIC_KEY" ]] \
    || die "Unable to parse keys produced by 'xray x25519'."

  jq -n --arg private "$REALITY_PRIVATE_KEY" --arg public "$REALITY_PUBLIC_KEY" --arg short "$REALITY_SHORT_ID" \
    '{private:$private,public:$public,short:$short}' >"${RUNTIME_DIR}/reality-keys.json.tmp"
  chmod 0600 "${RUNTIME_DIR}/reality-keys.json.tmp"
  mv "${RUNTIME_DIR}/reality-keys.json.tmp" "${RUNTIME_DIR}/reality-keys.json"
  load_reality_keys
}

load_reality_keys() {
  REALITY_PRIVATE_KEY=$(jq -er .private "${RUNTIME_DIR}/reality-keys.json")
  REALITY_PUBLIC_KEY=$(jq -er .public "${RUNTIME_DIR}/reality-keys.json")
  REALITY_SHORT_ID=$(jq -er .short "${RUNTIME_DIR}/reality-keys.json")
  printf '%s\n' "$REALITY_PRIVATE_KEY" >"${RUNTIME_DIR}/reality-private-key"
  printf '%s\n' "$REALITY_PUBLIC_KEY" >"${RUNTIME_DIR}/reality-public-key"
  printf '%s\n' "$REALITY_SHORT_ID" >"${RUNTIME_DIR}/reality-short-id"
  chmod 0600 "${RUNTIME_DIR}"/reality-*-key "${RUNTIME_DIR}/reality-short-id"
}

configure_xray() {
  CURRENT_STAGE="Xray configuration"
  log "Writing Xray configuration..."

  install -d -m 0750 -o root -g nogroup /usr/local/etc/xray
  # A crash after the atomic rename must not reset an existing client list.
  if ! jq -e '.inbounds[] | select(.tag == "vless-reality")' "$XRAY_CONFIG" >/dev/null 2>&1; then
  jq -n \
    --arg domain "$DUCKDNS_DOMAIN" \
    --arg privateKey "$REALITY_PRIVATE_KEY" \
    --arg shortId "$REALITY_SHORT_ID" \
    '{
      log: { loglevel: "warning" },
      inbounds: [
        {
          tag: "vless-reality",
          listen: "0.0.0.0",
          port: 443,
          protocol: "vless",
          settings: {
            clients: [],
            decryption: "none"
          },
          streamSettings: {
            network: "raw",
            security: "reality",
            realitySettings: {
              show: false,
              target: "127.0.0.1:8443",
              xver: 0,
              serverNames: [$domain],
              privateKey: $privateKey,
              shortIds: [$shortId]
            }
          }
        }
      ],
      outbounds: [
        { tag: "direct", protocol: "freedom" },
        { tag: "blocked", protocol: "blackhole" }
      ]
    }' >"${XRAY_CONFIG}.setup.tmp"
  /usr/local/bin/xray run -test -format json -config "${XRAY_CONFIG}.setup.tmp"
  mv "${XRAY_CONFIG}.setup.tmp" "$XRAY_CONFIG"
  fi
  chown root:nogroup "$XRAY_CONFIG"
  chmod 0640 "$XRAY_CONFIG"

  install -d -m 0755 /etc/systemd/system/xray.service.d
  cat >/etc/systemd/system/xray.service.d/vps-reality.conf <<EOF
[Unit]
Wants=network-online.target nginx.service
After=network-online.target nginx.service

EOF

  /usr/local/bin/xray run -test -format json -config "$XRAY_CONFIG"
}

update_dns() {
  CURRENT_STAGE="DuckDNS update"
  log "Updating the DuckDNS A record from this VPS..."
  local reply
  # No ip parameter: DuckDNS detects this VPS's outgoing IPv4, never the master's IP.
  reply=$(curl -4 -fsS --connect-timeout 10 --max-time 30 --get \
    --data-urlencode "domains=${DUCKDNS_DOMAIN%.duckdns.org}" \
    --data-urlencode "token@${RUNTIME_DIR}/duckdns-token" \
    https://www.duckdns.org/update) || die "DuckDNS is unreachable. Retry the installation."
  [[ "$reply" == "OK" ]] || die "DuckDNS rejected the update. Create this subdomain at https://www.duckdns.org and check its account token."
  log "DuckDNS accepted the VPS address."
}

run_stage() {
  local stage=$1
  if [[ -f "${RUNTIME_DIR}/setup-stages/${stage}" ]]; then
    log "Already completed: ${stage}"
  else
    "$stage"
    touch "${RUNTIME_DIR}/setup-stages/${stage}"
  fi
}

obtain_certificate() {
  CURRENT_STAGE="certificate issuance"
  log "Obtaining the initial Let's Encrypt certificate through DuckDNS DNS-01..."

  certbot certonly \
    --manual \
    --preferred-challenges dns \
    --manual-auth-hook "${INSTALL_DIR}/hooks/duckdns-auth.sh" \
    --manual-cleanup-hook "${INSTALL_DIR}/hooks/duckdns-cleanup.sh" \
    --non-interactive \
    --keep-until-expiring \
    --agree-tos \
    --email "$CERTBOT_EMAIL" \
    --cert-name "$DUCKDNS_DOMAIN" \
    -d "$DUCKDNS_DOMAIN"

  [[ -s "/etc/letsencrypt/live/${DUCKDNS_DOMAIN}/fullchain.pem" ]] \
    || die "Certbot did not create the expected fullchain.pem."
  [[ -s "/etc/letsencrypt/live/${DUCKDNS_DOMAIN}/privkey.pem" ]] \
    || die "Certbot did not create the expected privkey.pem."

  install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
  cat >/etc/letsencrypt/renewal-hooks/deploy/vps-reality-nginx.sh <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
/usr/sbin/nginx -t
/usr/bin/systemctl reload nginx.service
EOF
  chmod 0755 /etc/letsencrypt/renewal-hooks/deploy/vps-reality-nginx.sh

  systemctl cat certbot.timer >/dev/null 2>&1 \
    || die "Ubuntu's certbot.timer unit is missing."
  systemctl enable --now certbot.timer
}

configure_nginx() {
  CURRENT_STAGE="Nginx configuration"
  log "Configuring the camouflage site..."

  {
    cat <<EOF
server {
    listen 127.0.0.1:8443 ssl;
    server_name ${DUCKDNS_DOMAIN};

    ssl_certificate /etc/letsencrypt/live/${DUCKDNS_DOMAIN}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${DUCKDNS_DOMAIN}/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    location = / {
        root ${INSTALL_DIR};
        try_files /index.html =404;
    }

    location = /index.html {
        root ${INSTALL_DIR};
    }

EOF
    cat <<'EOF'

    location / {
        return 404;
    }
}
EOF
  } >"$NGINX_SITE"

  rm -f /etc/nginx/sites-enabled/default
  ln -sfn "$NGINX_SITE" "$NGINX_SITE_LINK"
  nginx -t
}

enable_services() {
  CURRENT_STAGE="service startup"
  log "Enabling system services..."
  systemctl daemon-reload

  systemctl enable nginx.service xray.service
  systemctl restart nginx.service
  systemctl restart xray.service

  systemctl is-active --quiet nginx.service || die "nginx.service is not active."
  systemctl is-active --quiet xray.service || die "xray.service is not active."
  systemctl is-active --quiet certbot.timer || die "certbot.timer is not active."
}

configure_firewall() {
  CURRENT_STAGE="firewall configuration"
  log "Allowing SSH and HTTPS through UFW..."

  local sshd_effective
  local ssh_port
  sshd_effective=$(sshd -T 2>/dev/null)
  ssh_port=$(awk '$1 == "port" {print $2; exit}' <<<"$sshd_effective")
  [[ "$ssh_port" =~ ^[0-9]+$ ]] || die "Unable to determine the current SSH port."

  ufw allow "${ssh_port}/tcp"
  ufw allow 443/tcp
  ufw --force enable
}

effective_sshd_config() {
  local address=$1
  sshd -T -C "user=root,host=localhost,addr=${address}"
}

check_sshd_value() {
  local config=$1
  local key=$2
  local expected_regex=$3
  local actual

  actual=$(awk -v wanted="$key" '$1 == wanted {$1=""; sub(/^ /, ""); print; exit}' <<<"$config")
  [[ "$actual" =~ $expected_regex ]]
}

validate_effective_sshd_config() {
  local address=$1
  local config
  config=$(effective_sshd_config "$address")

  check_sshd_value "$config" "allowusers" '^root$' \
    && check_sshd_value "$config" "pubkeyauthentication" '^yes$' \
    && check_sshd_value "$config" "authenticationmethods" '^publickey$' \
    && check_sshd_value "$config" "authorizedkeysfile" '^\.ssh/authorized_keys$' \
    && check_sshd_value "$config" "authorizedkeyscommand" '^none$' \
    && check_sshd_value "$config" "trustedusercakeys" '^none$' \
    && check_sshd_value "$config" "permitrootlogin" '^(prohibit-password|without-password)$' \
    && check_sshd_value "$config" "passwordauthentication" '^no$' \
    && check_sshd_value "$config" "kbdinteractiveauthentication" '^no$' \
    && check_sshd_value "$config" "permitemptypasswords" '^no$'
}

configure_ssh() {
  CURRENT_STAGE="SSH hardening"
  log "Restricting SSH access to root with the supplied public key..."

  install -d -m 0755 /etc/ssh/sshd_config.d
  cat >"$SSH_OVERRIDE" <<'EOF'
AllowUsers root
PubkeyAuthentication yes
AuthenticationMethods publickey
AuthorizedKeysFile .ssh/authorized_keys
AuthorizedKeysCommand none
TrustedUserCAKeys none
PermitRootLogin prohibit-password
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
EOF
  chmod 0644 "$SSH_OVERRIDE"

  if ! sshd -t; then
    rm -f "$SSH_OVERRIDE"
    die "The generated SSH configuration is invalid; the override was removed."
  fi

  if ! validate_effective_sshd_config "127.0.0.1"; then
    rm -f "$SSH_OVERRIDE"
    die "The SSH override did not become effective, probably because sshd_config contains an earlier directive."
  fi

  if [[ -n ${SSH_CONNECTION:-} ]]; then
    local client_address
    client_address=${SSH_CONNECTION%% *}
    if [[ -n "$client_address" ]] && ! validate_effective_sshd_config "$client_address"; then
      rm -f "$SSH_OVERRIDE"
      die "The SSH override is not effective for the current client address (${client_address})."
    fi
  fi

  install -d -m 0700 -o root -g root /root/.ssh
  touch /root/.ssh/authorized_keys
  grep -qxF -- "$SSH_PUBLIC_KEY" /root/.ssh/authorized_keys || printf '\n%s\n' "$SSH_PUBLIC_KEY" >>/root/.ssh/authorized_keys
  chown root:root /root/.ssh/authorized_keys
  chmod 0600 /root/.ssh/authorized_keys

  systemctl reload ssh.service
}

finish_installation() {
  CURRENT_STAGE="finalization"
  printf '2\n' >"${RUNTIME_DIR}/installation-version"
  touch "${RUNTIME_DIR}/initialized"
  chmod 0600 "${RUNTIME_DIR}/initialized" "${RUNTIME_DIR}/installation-version"
  log "Installation completed for ${DUCKDNS_DOMAIN}; management is SSH-only."
}

main() {
  parse_arguments "$@"
  CURRENT_STAGE="environment validation"
  check_environment
  run_stage install_packages
  install_assets
  prepare_runtime
  update_dns
  run_stage install_xray
  generate_reality_keys
  run_stage configure_xray
  run_stage obtain_certificate
  run_stage configure_nginx
  run_stage enable_services
  run_stage configure_firewall
  run_stage configure_ssh
  # Validate again even if a previous run completed all checkpoints.
  /usr/local/bin/xray run -test -format json -config "$XRAY_CONFIG"
  nginx -t
  systemctl start nginx.service xray.service certbot.timer
  systemctl is-active --quiet nginx.service xray.service certbot.timer
  finish_installation
}

main "$@"
