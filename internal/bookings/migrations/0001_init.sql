CREATE TABLE bookings.bookings (
    id                uuid PRIMARY KEY,
    owner_id          uuid        NOT NULL,
    sailing_id         uuid        NOT NULL,
    sailing_code       text        NOT NULL,
    reference         text        NOT NULL CHECK (char_length(reference) <= 40),
    teu             integer     NOT NULL,
    fee_cents         bigint      NOT NULL,
    status            text        NOT NULL,
    reason            text,
    bill_of_lading_number      text,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    capacity_released_at timestamptz,
    UNIQUE (sailing_id, reference)
);
CREATE INDEX bookings_owner_id ON bookings.bookings (owner_id);
CREATE INDEX bookings_created_at ON bookings.bookings (created_at DESC);

CREATE TABLE bookings.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX bookings_outbox_unsent ON bookings.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE bookings.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);

