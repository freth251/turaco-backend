#!/bin/bash
set -e

OS="$(uname -s)"
PSQL=""

find_psql() {
    if command -v psql &>/dev/null; then
        PSQL="psql"
    elif [ -f /opt/homebrew/opt/postgresql@17/bin/psql ]; then
        PSQL="/opt/homebrew/opt/postgresql@17/bin/psql"
    elif [ -f /usr/local/opt/postgresql@17/bin/psql ]; then
        PSQL="/usr/local/opt/postgresql@17/bin/psql"
    else
        return 1
    fi
}

install_postgres_macos() {
    echo "Installing PostgreSQL via Homebrew..."
    brew install postgresql@17
    brew services start postgresql@17
    sleep 3
}

install_postgres_linux() {
    echo "Installing PostgreSQL via apt..."
    sudo apt-get update -qq
    sudo apt-get install -y postgresql postgresql-contrib
    sudo systemctl enable postgresql
    sudo systemctl start postgresql
    sleep 3
}

start_postgres_linux() {
    if ! sudo systemctl is-active --quiet postgresql; then
        echo "Starting PostgreSQL..."
        sudo systemctl start postgresql
        sleep 3
    fi
}

start_postgres_macos() {
    if ! $PSQL postgres -c '\q' &>/dev/null; then
        echo "Starting PostgreSQL..."
        brew services start postgresql@17
        sleep 3
    fi
}

run_psql() {
    if [ "$OS" = "Linux" ]; then
        sudo -u postgres psql "$@"
    else
        $PSQL "$@"
    fi
}

echo "==> Checking for PostgreSQL..."
if ! find_psql; then
    if [ "$OS" = "Darwin" ]; then
        install_postgres_macos
    else
        install_postgres_linux
    fi
    find_psql
else
    echo "PostgreSQL found: ${PSQL:-$(which psql)}"
    if [ "$OS" = "Darwin" ]; then
        start_postgres_macos
    else
        start_postgres_linux
    fi
fi

echo "==> Ensuring postgres role has password set..."
run_psql postgres -tc "SELECT 1 FROM pg_roles WHERE rolname='postgres'" | grep -q 1 \
    && run_psql postgres -c "ALTER USER postgres PASSWORD 'password';" \
    || run_psql postgres -c "CREATE USER postgres WITH SUPERUSER PASSWORD 'password';"

echo "==> Creating database turacodb (if not exists)..."
run_psql postgres -tc "SELECT 1 FROM pg_database WHERE datname='turacodb'" | grep -q 1 \
    || run_psql postgres -c "CREATE DATABASE turacodb;"

echo "==> Creating tables..."
run_psql -U postgres turacodb <<'SQL'
CREATE TABLE IF NOT EXISTS contacts (
    id           SERIAL PRIMARY KEY,
    email        VARCHAR(100) NOT NULL,
    name         VARCHAR(100) NOT NULL,
    phone_number VARCHAR(15),
    message      TEXT NOT NULL,
    created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS reservations (
    id           SERIAL PRIMARY KEY,
    name         VARCHAR(255) NOT NULL,
    email        VARCHAR(255) NOT NULL,
    phone_number VARCHAR(20),
    guests       INTEGER NOT NULL,
    check_in     TIMESTAMP NOT NULL,
    check_out    TIMESTAMP NOT NULL,
    room_type    TEXT,
    created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS page_views (
    id         SERIAL PRIMARY KEY,
    ip         VARCHAR(45),
    user_agent TEXT,
    page       VARCHAR(500),
    referrer   VARCHAR(500),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
SQL

echo ""
echo "Setup complete. Database 'turacodb' is ready."
echo "Run 'go run main.go' to start the server."
