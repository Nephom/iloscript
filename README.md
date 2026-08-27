# iloscript
HPE iLO RestAPI script with Golang

go build
./iloscript -h

# Setup
Copy `.env.example` to `.env` and fill in `ILO_USERNAME` / `ILO_PASSWORD`
(or export them as environment variables). `.env` is gitignored.

# Version
1. 1.1.x: This script only for HPE iLO 5/6/7 using.
2. 1.2.x: Update for HPE ILO 7 above using.
