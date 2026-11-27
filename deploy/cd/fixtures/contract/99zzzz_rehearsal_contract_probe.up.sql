-- rehearsal:contract
-- Stand-in for a contract-phase migration, used only by the X99 cutover
-- rehearsal (deploy/cd/rehearse-cutover-x99.sh). It sorts after every
-- migration in server/migrations and is copied into the rehearsal's own tree,
-- never into the repository's migration directory.
CREATE TABLE rehearsal_contract_probe (id integer PRIMARY KEY);
INSERT INTO rehearsal_contract_probe (id) VALUES (1);
