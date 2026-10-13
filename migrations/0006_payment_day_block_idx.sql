create index payment_day_block_idx on payment (chain_id, day, block, log_index);
drop index payment_day_idx;
