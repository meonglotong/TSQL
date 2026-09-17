%global debug_package %{nil}

Name:           tsql
Version:        0.5.1
Release:        1%{?dist}
Summary:        TSQL (tamamiSQL): a small relational SQL database server in Go
License:        MIT
URL:            https://github.com/meonglotong/TSQL
Source0:        %{name}-%{version}.tar.gz

# The static binaries are built with the system Go toolchain present on the
# build host (CGO_ENABLED=0, so the package has no Go runtime dependency).
# BuildRequires: golang

%description
TSQL (tamamiSQL) is a small but real relational SQL database server written
in Go, inspired by PostgreSQL. It speaks its own framed JSON protocol (with a
psql-like CLI, tsql) and supports SQL DDL/DML, INNER JOIN, GROUP BY with
aggregates, UNIQUE secondary indexes, explicit transactions (BEGIN/COMMIT/
ROLLBACK), foreign keys with RESTRICT behavior, and crash-safe WAL + snapshot
persistence.

%prep
%setup -q

%build
export CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o tsqld ./cmd/tsqld
go build -trimpath -ldflags "-s -w" -o tsql ./cmd/tsql

%install
rm -rf %{buildroot}
install -d -m 0755 %{buildroot}%{_bindir}
install -d -m 0755 %{buildroot}%{_unitdir}
install -m 0755 tsqld %{buildroot}%{_bindir}/tsqld
install -m 0755 tsql %{buildroot}%{_bindir}/tsql
install -m 0644 packaging/systemd/tsqld.service %{buildroot}%{_unitdir}/tsqld.service

%pre
getent group tsql >/dev/null 2>&1 || groupadd -r tsql
getent passwd tsql >/dev/null 2>&1 || useradd -r -g tsql -d /var/lib/tsql -s /sbin/nologin tsql

%post
if [ $1 -eq 1 ]; then
  # Fresh install: create the data directory and start the service.
  install -d -o tsql -g tsql -m 0750 /var/lib/tsql
  if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    systemctl enable --now tsqld.service || true
  fi
else
  # Upgrade: reload the unit and restart only if it is running.
  if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    systemctl try-restart tsqld.service || true
  fi
fi
exit 0

%preun
if [ $1 -eq 0 ] && [ -d /run/systemd/system ]; then
  systemctl disable --now tsqld.service || true
fi
exit 0

%postun
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
fi
# The data directory /var/lib/tsql is deliberately kept on uninstall.
exit 0

%files
%attr(0755, root, root) %{_bindir}/tsqld
%attr(0755, root, root) %{_bindir}/tsql
%attr(0644, root, root) %{_unitdir}/tsqld.service

%license LICENSE
