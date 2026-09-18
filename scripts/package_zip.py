#!/usr/bin/env python3
"""Create a release ZIP with portable UTF-8 member names."""

import argparse
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile


def write_zip(source: Path, destination: Path) -> None:
    # zipfile writes the UTF-8 flag for non-ASCII names; Apple's zip does not.
    with ZipFile(destination, "w", compression=ZIP_DEFLATED, strict_timestamps=False) as archive:
        for path in sorted(source.rglob("*")):
            archive.write(path, path.relative_to(source.parent).as_posix())


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    if not args.source.is_dir():
        parser.error("source must be a staging directory")
    write_zip(args.source, args.destination)
