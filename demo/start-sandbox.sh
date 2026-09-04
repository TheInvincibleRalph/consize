#!/bin/bash
set -e

echo "=========================================================="
echo " Starting Consize Interactive Sandbox"
echo "=========================================================="

# 1. Initialize and start PostgreSQL
echo "--> Initializing database..."
mkdir -p /var/lib/postgresql/data
chown -R postgres:postgres /var/lib/postgresql/data

su-exec postgres initdb -D /var/lib/postgresql/data > /dev/null
echo "host all all 127.0.0.1/32 trust" >> /var/lib/postgresql/data/pg_hba.conf

echo "--> Starting PostgreSQL..."
su-exec postgres pg_ctl -D /var/lib/postgresql/data -l /var/lib/postgresql/logfile start > /dev/null

# Wait for Postgres to be ready
until su-exec postgres pg_isready -h 127.0.0.1 -q; do
  sleep 1
done

# Create user and db
su-exec postgres psql -c "CREATE USER consize WITH PASSWORD 'consize';" > /dev/null 2>&1 || true
su-exec postgres psql -c "CREATE DATABASE consize OWNER consize;" > /dev/null 2>&1 || true

# 2. Run migrations and seed data
echo "--> Running migrations and seeding fixture data..."
migrate
devseed

# 3. Start the services in the background
echo "--> Starting services..."
prometheus-stub &
api &
sh -c "while true; do verify; sleep 10; done" &
injector &

# 4. Start Next.js UI
echo "--> Starting UI..."
cd /app
node server.js &

echo ""
echo "=========================================================="
echo " Sandbox is ready!"
echo " Open http://localhost:3000 in your browser"
echo "=========================================================="
echo " (Press Ctrl+C to stop)"
echo ""

# Wait for any background process to fail or exit
wait -n
