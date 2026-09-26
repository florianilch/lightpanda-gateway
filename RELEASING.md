# Releasing

Signed `vX.Y.Z` tags are the release authority. Pushing a tag triggers GitHub Actions to build binary archives, generate checksums/SBOMs and stage a GitHub Release.

## Steps

1. Choose version `X.Y.Z`.

2. Update any documentation or `CHANGELOG.md` for version `vX.Y.Z`:

3. Commit the release changes to `main`:

   ```sh
   git commit -m "chore(release): prepare for vX.Y.Z"
   git push origin main
   ```

4. Create a signed tag on that commit and push it:

   ```sh
   git tag -s vX.Y.Z -m "Release vX.Y.Z"
   git push origin vX.Y.Z
   ```

5. GitHub Actions builds the release artifacts and stages a draft GitHub Release. Review and publish the draft release.
