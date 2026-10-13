create table daily_rules (
    chain_id int not null,
    day      int not null,
    rule     text not null,
    payments bigint not null,
    primary key (chain_id, day, rule)
);
