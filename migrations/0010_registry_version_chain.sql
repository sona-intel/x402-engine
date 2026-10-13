alter table registry_version add column chain_id int;
update registry_version v set chain_id = coalesce((select m.chain_id from meta m where m.registry_version = v.version limit 1), 143);
alter table registry_version alter column chain_id set not null;
alter table registry_version drop constraint registry_version_pkey;
alter table registry_version add primary key (chain_id, version);
