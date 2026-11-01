#!/bin/sh
set -eu

password_file=${PASSPORT_RABBITMQ_PASSWORD_FILE:-}
if [ -z "$password_file" ] || [ ! -r "$password_file" ]; then
    echo "RabbitMQ password secret unavailable" >&2
    exit 1
fi

RABBITMQ_DEFAULT_PASS=$(cat "$password_file")
if [ -z "$RABBITMQ_DEFAULT_PASS" ]; then
    echo "RabbitMQ password secret is empty" >&2
    exit 1
fi
export RABBITMQ_DEFAULT_PASS
unset PASSPORT_RABBITMQ_PASSWORD_FILE password_file

exec /usr/local/bin/docker-entrypoint.sh "$@"
