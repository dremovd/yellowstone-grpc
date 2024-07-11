How to parse a transaction:
1. List all the amounts change (post - pre) from meta.pre_token_balances and meta.post_token_balances. We need to extract: mint, owner, and amount. Mint and onwer are base58 encoded, amount is int represented as a str. 
2. List concatenated transaction.message.account_keys, meta.loaded_writable_addresses, meta.loaded_readonly_addresses as a list (in this order), encode them in base58. Let's name it account_keys_extended
3. Get transaction signer, which is always in a transaction.message.account_keys[0]
4. Get transaction fee from meta.fee
5. Get signature from signature
6. Get transaction.message.instructions and filter only the first one with program_id JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4 (program_id_index is pointing to account_keys_extended).
7. Filter only meta.inner_instructions related to the index transaction.message.instructions we filtered in a previous step. It's related to the index from message.instructions
8. For inner instructions we know:
a. Accounts are indexes of values from account_keys_extended
b. Program id index is also from account_keys_extended
c. There are pairs of instructions with TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA program id we need to look for. These are swap instructions.
d. First one in a pair is usually corresponds to amount signer is transferring, while second one is for token amount of other token he gets. 
e. We need to extract the value from data and also accounts for these instructions.
f. From data we need to extract their value, it's located in data at 2:18 bytes and should be interpreted as LE unsigned int.
g. We also need to extract program id and accounts from instrunction preceding this pair. This preceding instruction should be not TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA program. If it is, we should skip this pair altogether.

A result should be a key-value dict that is serialized as a JSON and added as a new column.

Here is a rought exampe of such dict:
{
    "diff_mint_0": base58 string,
    "diff_owner_0": base58 string,
    "diff_amount_0": signed int64,
    "diff_mint_1": base58 string,
    "diff_owner_1": base58 string,
    "diff_amount_1": signed int64,
    ...
    "diff_mint_n": base58 string,
    "diff_owner_n": base58 string,
    "diff_amount_n": signed int64,
    "accounts_keys_extended_0": base58 string,
    "accounts_keys_extended_1": base58 string,
    ...
    "accounts_keys_extended_m": base58 string,
    "signer": base58 string,
    "fee": uint64,
    "signature": base58 string,
    "instruction_out_amount_0": uint64,
    "instruction_out_accounts_0_0": base58 string,
    "instruction_out_accounts_0_1": base58 string,
    ...
    "instruction_out_accounts_0_p": base58 string,
    "instruction_in_amount_0": uint64,
    "instruction_in_accounts_0_1": base58 string,
    ...
    "instruction_in_accounts_0_q": base58 string,
    "instruction_preceeding_program_id_0": base58 string,
    "instruction_preceeding_accounts_0_0": base58 string,
    "instruction_preceeding_accounts_0_1": base58 string,
    ...
    "instruction_preceeding_accounts_0_k": base58 string,
    (the same for 2nd pair and so on)
}