# Architecture diagram

The overview in the [README](../../README.md#how-it-works) is generated with [Archify](https://github.com/tt-a1i/archify) from [overview.json](overview.json).

- `overview.json` is the source. Every component cites the spec section, ADR or code that justifies it, pinned to a commit in `meta.repository.revision`.
- `overview-light.png` and `overview-dark.png` are what the README shows.
- The interactive HTML (source links, component details, milestone tags) is not committed, because it is large and regenerated from the JSON.

## Regenerating

Update the diagram when a milestone changes the architecture or its status tags.

1. Edit `overview.json`, and set `meta.repository.revision` to a commit on `main` that contains every cited line.
2. Render and check it from the repository root:

   ```sh
   node <archify>/bin/archify.mjs finalize architecture docs/architecture/overview.json \
     docs/architecture/overview.html --repo-root . --quality showcase
   ```

3. Capture the light and dark images with `archify visual-check`, crop them to the diagram, and replace the two PNGs.
