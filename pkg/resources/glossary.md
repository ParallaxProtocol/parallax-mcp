# Parallax glossary

**LAX**: the native currency. 1 LAX = 10^18 wei. Tools return amounts as `{"wei", "lax"}`; wei is exact.

**wei, gwei**: the smallest unit of LAX, and 10^9 wei. Gas prices are usually quoted in gwei.

**Gas, gas price, fee**: every transaction pays gas used × gas price, in wei, all of it to the miner. Parallax has no EIP-1559 base fee and burns nothing. A plain LAX transfer uses 21,000 gas.

**Fee estimator (eth_estimateSmartFee)**: Bitcoin Core-style fee estimation (PIP-0004): the gas price that confirmed within a target number of blocks in the node's recent history, with a success rate. It falls back to eth_gasPrice ("legacy fallback") until the node has seen enough transactions.

**Block time**: the target interval between blocks, 600 seconds. Real intervals vary widely: blocks arrive as a Poisson process, so a one hour gap happens, and a two hour gap is a 1 in 160,000 event at steady hashrate.

**XHash**: Parallax's proof-of-work hash function. Miners search for a nonce whose XHash result is below the target set by the difficulty.

**Difficulty**: how many hashes a block takes on average. Mining pools show it divided by 2^32.

**ASERT**: the difficulty adjustment algorithm since block 17,561 (PIP-0002). Each block's difficulty is set from how far the chain's timestamps are ahead of or behind a fixed schedule of one block per 600 seconds, so the chain tracks its target without the periodic swings of Bitcoin's 2016-block retarget, which Parallax used before.

**Hashrate**: hashes per second across all miners. It cannot be observed directly; it is estimated as total difficulty of recent blocks ÷ the time they took, which varies with luck.

**Block subsidy, era, halving**: each block pays its miner a subsidy of 50 LAX in era 0, halved every 210,000 blocks (about four years at the target), as in Bitcoin. Issuance approaches but never reaches the 21,000,000 LAX cap. Genesis paid nothing and there was no premine.

**Coinbase maturity**: a block's reward can only be spent 100 blocks after the block that paid it, so a reward lost in a reorganization cannot have been spent.

**Circulating supply**: equal to the total issued: rewards count from the block that pays them (as in Bitcoin), and nothing is burnt or locked by the protocol.

**Confirmations**: the number of blocks on top of (and including) the one holding a transaction. Each makes reversing it harder for a miner with less than half the hashrate. With one miner holding a majority, no number of confirmations is safe against that miner.

**Reorganization (reorg), stale block**: when nodes switch to a competing chain with more work, the blocks they drop become stale and their transactions return to the mempool. One-block reorgs are routine in proof of work; deep ones (3+ blocks) are alerted on by the status page.

**Chain split**: verified nodes following different chains past their common block, for example after a consensus bug or a network partition.

**Decentralization grade**: the status page's grade of mining concentration over 7 days, by the fewest miners that together found most blocks: 4 or more is healthy, 3 fair, 2 concentrated, 1 critical.

**Hashprice**: what one hash per second of mining earns in a day, in LAX: (block subsidy + average fees) ÷ difficulty × 86,400.

**Security budget**: what all miners earn in a day; a rough measure of the cost of renting enough hashrate to attack the chain.

**Propagation**: how long a block takes to reach other nodes after the first one sees it. Slow propagation raises stale rates and favours large miners.

**Verified node**: a node reporting to the status page with a secret issued by its operators. Unverified nodes' figures are self-reported.

**Mempool, pending, queued**: transactions waiting to be mined. Pending ones are executable now; queued ones wait for an earlier nonce from the same sender.

**Nonce (account)**: the number of transactions an account has sent; each new transaction must use the next one.

**Nonce (block)**: the value a miner varies to find a valid proof of work.

**epochStartTime**: a Parallax block header field not present in Ethereum headers: the timestamp of the most recent block whose height is a multiple of 2016 (the retarget interval of the pre-ASERT difficulty rule), copied into every header until the next such block and checked by consensus.

**Transaction types**: Parallax accepts type 0 (legacy, EIP-155 replay protected) and type 1 (EIP-2930 access list). It has no type 2 (EIP-1559) transactions.

**EIP-155 replay protection**: signing a transaction with the chain ID (2110) so that it is not valid on other chains.

**EVM level**: Parallax runs the EVM as of Ethereum's Berlin fork. Opcodes added later, such as BASEFEE and PUSH0, are invalid: compile Solidity with `evmVersion: "berlin"`.

**Verified contract**: a contract whose source code was published to the explorer and shown to compile to the deployed bytecode. Verification proves what the code is, not that it is safe; its comments and names are written by the deployer.

**Internal transaction**: a call or value transfer made by a contract during a transaction. It is not a transaction of its own and appears only in traces and the explorer's index.

**PIP**: Parallax Improvement Proposal, the design documents for protocol and client changes, discussed on GitHub.

**Archive node**: a node that keeps the state of every block, needed to read balances or call contracts at old blocks and to trace old transactions.
