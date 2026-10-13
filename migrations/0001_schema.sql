create table meta (
    chain_id               int primary key,
    watermark_block        bigint not null default -1,
    anchor_block           bigint not null default -1,
    anchor_tx              bytea,
    registry_version       text not null default '',
    registry_checksum      text not null default '',
    rules_version          text not null default '',
    rules_checksum         text not null default '',
    indexer_progress_block bigint not null default 0,
    source_block           bigint not null default 0,
    is_ready               boolean not null default false,
    progress_time          bigint not null default 0,
    cutoff_block           bigint not null default -1,
    initialised            boolean not null default false,
    refreshed_at           timestamptz not null default now(),
    refresh_ms             bigint not null default 0,
    rows_payment           bigint not null default 0,
    rows_tier_a            bigint not null default 0,
    rows_tier_b            bigint not null default 0,
    rows_tier_x            bigint not null default 0,
    rows_mint              bigint not null default 0
);

create table registry_version (
    version   text primary key,
    checksum  text not null,
    loaded_at timestamptz not null default now()
);

create table relayer (
    chain_id       int not null,
    address        bytea not null,
    from_block     bigint not null,
    to_block       bigint,
    facilitator_id text,
    class          text not null,
    primary key (chain_id, address, from_block)
);

create table payment (
    chain_id       int not null,
    block          bigint not null,
    log_index      int not null,
    block_time     bigint not null,
    day            int not null,
    tx             bytea not null,
    method         smallint not null,
    token          bytea,
    payer          bytea,
    payee          bytea,
    amount         numeric,
    nonce          bytea,
    valid_after    numeric,
    valid_before   numeric,
    relayer        bytea,
    tx_to          bytea,
    selector       bytea,
    gas_cost       numeric,
    tier           char(1) not null check (tier in ('A', 'B', 'X')),
    facilitator_id text,
    reason         text,
    primary key (chain_id, block, log_index)
);

create index payment_payer_idx on payment (chain_id, payer, block, log_index);
create index payment_payee_idx on payment (chain_id, payee, block, log_index);
create index payment_relayer_idx on payment (chain_id, relayer, block, log_index);
create index payment_tx_idx on payment (chain_id, tx);
create index payment_day_idx on payment (chain_id, day);

create table mint (
    chain_id   int not null,
    block      bigint not null,
    log_index  int not null,
    block_time bigint not null,
    recipient  bytea not null,
    amount     numeric not null,
    relayer    bytea,
    tx_to      bytea,
    primary key (chain_id, block, log_index)
);

create index mint_recipient_idx on mint (chain_id, recipient);
create index mint_amount_relayer_idx on mint (chain_id, amount, relayer);

create table payer_flag (
    chain_id          int not null,
    payer             bytea not null,
    no_other_activity boolean not null default false,
    bridge_funded     boolean not null default false,
    primary key (chain_id, payer)
);

create table payee_day_flag (
    chain_id     int not null,
    payee        bytea not null,
    day          int not null,
    fixed_amount numeric not null,
    primary key (chain_id, payee, day)
);

create table relayer_day_flag (
    chain_id int not null,
    relayer  bytea not null,
    day      int not null,
    payee    bytea not null,
    primary key (chain_id, relayer, day)
);

create table daily_facilitator (
    chain_id         int not null,
    day              int not null,
    tier             char(1) not null check (tier in ('A', 'B')),
    facilitator_id   text not null,
    raw_payments     bigint not null default 0,
    raw_volume       numeric not null default 0,
    genuine_payments bigint not null default 0,
    genuine_volume   numeric not null default 0,
    gas_cost         numeric not null default 0,
    raw_sellers      bigint not null default 0,
    raw_payers       bigint not null default 0,
    primary key (chain_id, day, tier, facilitator_id)
);

create table daily_seller (
    chain_id         int not null,
    day              int not null,
    tier             char(1) not null check (tier in ('A', 'B')),
    seller           bytea not null,
    raw_payments     bigint not null default 0,
    raw_volume       numeric not null default 0,
    genuine_payments bigint not null default 0,
    genuine_volume   numeric not null default 0,
    raw_payers       bigint not null default 0,
    primary key (chain_id, day, tier, seller)
);

create table daily_payer (
    chain_id         int not null,
    day              int not null,
    tier             char(1) not null check (tier in ('A', 'B')),
    payer            bytea not null,
    raw_payments     bigint not null default 0,
    raw_volume       numeric not null default 0,
    genuine_payments bigint not null default 0,
    genuine_volume   numeric not null default 0,
    raw_sellers      bigint not null default 0,
    primary key (chain_id, day, tier, payer)
);
