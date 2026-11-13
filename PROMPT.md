# Build prompt: Lazarette

Open a new session on this repository and give it everything below the line.

---

You are building **Lazarette**, a control-plane service of the ClearedStack platform.

**What it is for:** Keeps built images inside the cluster, with recent versions ready for rollback.

Read `SPEC.md` from start to finish before you do anything else. It is the specification. This prompt tells you how to work. Then read `component.json`, which is this component’s entry in the naming registry.

Nothing has been built. This repository holds the specification and no code.

## Where it sits

- **Namespace:** `clearedstack`
- **Depends on:** nothing else in the platform
- **Depended on by:** Keelworks and Shipkeeper

Its boundaries:

- Standard OCI registry API, inside the cluster only
- Pushes from Keelworks's build jobs
- Pulls by the cluster's container runtime when Shipkeeper deploys

## What to build

Everything `SPEC.md` describes, and nothing it does not. Where the specification gives a table of interfaces, failure modes or security rules, each row is a requirement.

## What does not exist yet

Lazarette depends on nothing else in the platform, so it can be built and tested entirely alone. Other components will call it later. Do not build them here.

## Rules

1. **Language:** Go, using the standard library wherever it is enough.
2. **Clean room.** Write everything from scratch. Do not copy code, text or structure from another project, however permissive its license. An open-source library may be used as a dependency if its license is MIT, BSD or Apache 2.0 and its notice is kept.
3. **No AI when it runs.** No model, and no call to one. The same input must give the same output on any machine, every time.
4. **Stop instead of guessing.** When something is unexpected, refuse and report it plainly. Never improvise.
5. **Every rule has a saved example.** Each rule ships with fixtures: a saved input and the exact output expected. A change in behaviour must fail a test.
6. **Small and unprivileged.** It has to run as an ordinary user with a read-only filesystem, no added privileges, and within 64 MiB of memory.
7. **No Docker.** The image is built straight from source with `ko`. Do not add a Dockerfile.
8. **Three public error codes.** `E_TIMEOUT` (something outside took too long), `E_PARSE` (the data was broken), `E_NEEDS_SUBTOOL` (the data was valid and unfamiliar). Nothing that names a machine, an address or a credential may appear in an error.
9. **Made-up data only.** No classified or controlled information, no real contract data, no real rates, no real people, anywhere: code, fixtures, comments, commit messages.
10. **MIT license.** Keep the `LICENSE` file.
11. **Names come from the registry.** This component is `lazarette`. Use the names in `component.json` for everything it touches. Do not invent an identifier.

## How to start

1. Read `SPEC.md` and `component.json`.
2. Reply with a short plan before writing any code: the layout of the repository, the first milestone, and every place where the specification is unclear, contradicts itself or cannot be built as written.
3. Wait for the plan to be agreed.
4. Build the first milestone, with its tests, and stop for review.

**Suggested first milestone:** Push and pull of one image by digest through the standard registry API, stored on disk, proven with an existing client and not a home-made one.

## If the specification is wrong

Say so, and propose a change to `SPEC.md` with the reason. Do not quietly build something different from what it says.

## Done means

- Every requirement in `SPEC.md` is either built and tested, or listed in the README as not yet done.
- The tests pass, and they include the saved examples.
- The README says what works today, in plain words, and how to run it.
- The status line in the README is changed from “design” only when that is true.
