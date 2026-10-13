create table daily_flow (
    chain_id         int not null,
    day              int not null,
    tier             char(1) not null check (tier in ('A', 'B')),
    facilitator_id   text not null,
    payee            bytea not null,
    raw_payments     bigint not null default 0,
    raw_volume       numeric not null default 0,
    genuine_payments bigint not null default 0,
    genuine_volume   numeric not null default 0,
    primary key (chain_id, day, tier, facilitator_id, payee)
);
