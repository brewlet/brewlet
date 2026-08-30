from hashlib import sha256
from pathlib import Path


STYLESHEET = Path("stylesheets/brewlet-docs.css")


def version_stylesheet(site_dir: Path) -> Path:
    source = site_dir / STYLESHEET
    content = source.read_bytes()
    digest = sha256(content).hexdigest()[:12]
    versioned = source.with_name(f"{source.stem}.{digest}{source.suffix}")
    versioned.write_bytes(content)

    reference = STYLESHEET.as_posix()
    versioned_reference = reference.replace(source.name, versioned.name)
    replacements = 0

    for page in site_dir.rglob("*.html"):
        html = page.read_text(encoding="utf-8")
        updated = html.replace(reference, versioned_reference)
        if updated != html:
            page.write_text(updated, encoding="utf-8")
            replacements += 1

    if replacements == 0:
        raise RuntimeError(f"No HTML pages referenced {reference}")

    return versioned


def on_post_build(*, config, **_kwargs) -> None:
    version_stylesheet(Path(config.site_dir))
