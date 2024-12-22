# starknet-batcher

This repository gathers the code mentionned in the article [A Starknet transactions batcher](TODO).

![Architecture](./batcher.png)

## CLI tool

One can use the batcher as a CLI tool to send a bunch of NFT transactions. First, create a `.env` file as the `template.env`:

```bash
RPC_URL=""
ACCOUNT_ADDRESS=""
ACCOUNT_CAIRO_VERSION=""
NFT_CONTRACT_ADDRESS=""
```

Then, run the following commands:

```bash
go run main.go <private_key> <csv_file>
```

> [!WARNING]
> Be cautious with your private key. It should be kept secret. Please take all necessary precautions to protect it if you use the tool in a production environment.
> It might be better to experiment with the tool on a brand new account with just a few funds.

The CSV file should have the following format:

```csv
<to_address>,<token_id>
```
