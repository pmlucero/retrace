# Architecture diagram

The overview in the [README](../../README.md#how-it-works) is generated with [Archify](https://github.com/tt-a1i/archify) **3.2.0** from [overview.json](overview.json).

- `overview.json` is the source. Every component cites the spec section, ADR or code that justifies it, pinned to a commit in `meta.repository.revision`.
- `overview-light.png` and `overview-dark.png` are what the README shows.
- The interactive HTML (source links, component details, milestone tags) is not committed, because it is large and regenerated from the JSON.

Nothing checks automatically that the images still match the JSON. Whoever changes `overview.json` regenerates both images in the same PR.

CI (`.github/scripts/check_docs.py`) checks that `overview.json` parses, that `meta.repository.revision` is a full commit SHA on `main`, and that every cited path and line range exists at that revision. It keeps the permalinks valid. It does not notice that the code at `HEAD` has moved away from a citation, so re-check the cited lines when you re-pin.

## Regenerating

Update the diagram when a milestone changes the architecture or its status tags. Use Archify 3.2.0; a newer version can change the JSON schema or the rendering, so upgrading it is a change of its own.

1. Edit `overview.json`, and set `meta.repository.revision` to a commit on `main` that contains every cited line.
2. Render and check it from the repository root:

   ```sh
   node <archify>/bin/archify.mjs finalize architecture docs/architecture/overview.json \
     docs/architecture/overview.html --repo-root . --quality showcase
   ```

3. Capture the light and dark screenshots:

   ```sh
   node <archify>/bin/archify.mjs visual-check docs/architecture/overview.html --summary --out-dir <tmp>
   ```

4. Crop the 2048×1320 captures to the diagram and its legend, without the viewer's title bar and controls. With the current canvas (1232 px wide) that is 1450×1050 px from offset (300, 150). On macOS:

   ```sh
   for t in light dark; do
     sips -c 1050 1450 --cropOffset 150 300 <tmp>/overview.visual-check.2048x1320.$t.png \
       --out docs/architecture/overview-$t.png
   done
   ```

   If the canvas size changes, adjust the crop so it keeps every box and the legend, and check both images before committing.
