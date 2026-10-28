CREATE TABLE capacity.sailings (
    id          uuid PRIMARY KEY,
    code        text        NOT NULL UNIQUE CHECK (char_length(code) <= 20),
    vessel     text        NOT NULL,
    origin     text        NOT NULL,
    destination     text        NOT NULL,
    departure     timestamptz NOT NULL,
    capacity_teu integer     NOT NULL,
    remaining_teu  integer     NOT NULL,
    created_at  timestamptz NOT NULL,
    CONSTRAINT sailings_remaining CHECK (remaining_teu >= 0 AND remaining_teu <= capacity_teu)
);

CREATE TABLE capacity.teu_holds (
    booking_id       uuid PRIMARY KEY,
    owner_id       uuid        NOT NULL,
    sailing_id        uuid        NOT NULL REFERENCES capacity.sailings (id),
    teu          integer     NOT NULL,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text
);

CREATE TABLE capacity.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX sailings_outbox_unsent ON capacity.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE capacity.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);
