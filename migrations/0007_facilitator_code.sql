create table facilitator_code (
    chain_id       int not null,
    code           text not null,
    facilitator_id text not null,
    primary key (chain_id, code)
);
