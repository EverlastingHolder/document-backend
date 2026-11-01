FROM rabbitmq:4.4.0-management-alpine

COPY entrypoint.sh /usr/local/bin/passport-rabbitmq-entrypoint.sh
RUN chmod 0755 /usr/local/bin/passport-rabbitmq-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/passport-rabbitmq-entrypoint.sh"]
CMD ["rabbitmq-server"]
