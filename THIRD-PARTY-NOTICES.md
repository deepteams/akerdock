# Third-party notices

AkerDock is distributed under the Apache License 2.0 (see `LICENSE`). It compiles
third-party Go modules into its binary and bundles third-party assets into its
embedded web dashboard (`internal/web/dist`). This file collects the attribution
notices for those redistributed components.

> The exhaustive, machine-generated set of dependency license texts (every Go
> module in the build graph and every npm package in the production bundle) is
> produced at **release time** with `go-licenses save` and `syft`, and shipped
> with the release artifacts and published images — see
> `docs/specs/licensing-sbom.md` §1.1 / §6. This file is the human-readable,
> curated companion, and carries in full the licenses that require the notice to
> travel with the distributed asset (the bundled fonts and icons).

All redistributed dependencies are under permissive licenses (MIT, BSD-2/3-Clause,
Apache-2.0, ISC, 0BSD, MPL-2.0, SIL OFL-1.1). No GPL / LGPL / AGPL / SSPL / BUSL /
non-commercial code is linked into the binary or bundled into the UI.

---

## 1. Go modules (compiled into the `akerdock` binary)

Licenses present in the build graph, by SPDX identifier: **MIT, BSD-2-Clause,
BSD-3-Clause, Apache-2.0, ISC, MPL-2.0, BlueOak-1.0.0, 0BSD**. Notable components:

- **Go standard library and `golang.org/x/*`** — BSD-3-Clause — © The Go Authors.
- **jackc/pgx** (PostgreSQL driver) — MIT.
- **go-chi/chi** (HTTP router) — MIT.
- **spf13/cobra**, **spf13/pflag** — Apache-2.0.
- **coder/websocket** — ISC.
- **OpenTelemetry Go** (`go.opentelemetry.io/otel*`, contrib) — Apache-2.0 — © The
  OpenTelemetry Authors. Their upstream `NOTICE` files are aggregated into the
  release `THIRD-PARTY-NOTICES` bundle.
- **prometheus/client_golang** — Apache-2.0.
- **compose-spec/compose-go** — Apache-2.0.
- **oapi-codegen/runtime** — Apache-2.0.
- **go-webauthn/webauthn** — BSD-3-Clause.
- **gopkg.in/yaml.v3** — MIT / Apache-2.0.

Two MPL-2.0 modules appear in the module graph — **go-sql-driver/mysql** and
**hashicorp/golang-lru** — but only through the build/codegen tools (sqlc, goose);
they are **not** linked into the `akerdock` binary and therefore not redistributed
in it. The full per-module license texts are generated at release (`go-licenses
save`).

---

## 2. Web dashboard bundle (embedded in `internal/web/dist`)

- **Angular** (`@angular/*`) — MIT — © Google LLC.
- **zone.js** — MIT — © 2010-2025 Google LLC.
- **RxJS** — Apache-2.0 — © the RxJS contributors (same text as `LICENSE`).
- **tslib** — 0BSD — © Microsoft Corporation.
- **xterm.js** (`@xterm/xterm`, `@xterm/addon-fit`) — MIT — © 2017-2019 The
  xterm.js authors; © 2014-2016 SourceLair Private Company; © 2012-2013
  Christopher Jeffrey.
- **Lucide** icons (`lucide-angular`) — ISC — see §2.1.
- **IBM Plex Sans**, **JetBrains Mono**, **Space Grotesk** fonts
  (`@fontsource/*`) — SIL Open Font License 1.1 — see §2.2. These font files ship
  as `.woff`/`.woff2` under `internal/web/dist/media/`; the OFL requires this
  license to accompany them.

### 2.1 ISC License — Lucide

    Copyright (c) for portions of Lucide are held by Cole Bemis 2013-2022 as part
    of Feather (MIT). All other copyright (c) for Lucide are held by Lucide
    Contributors 2022.

    Permission to use, copy, modify, and/or distribute this software for any
    purpose with or without fee is hereby granted, provided that the above
    copyright notice and this permission notice appear in all copies.

    THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
    REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY
    AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
    INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
    LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR
    OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
    PERFORMANCE OF THIS SOFTWARE.

### 2.2 SIL Open Font License, Version 1.1 — bundled fonts

Applies to the following font families, each retaining its own copyright line:

- IBM Plex Sans — Copyright 2019 IBM Corp. (https://github.com/IBM/plex)
- JetBrains Mono — Copyright 2020 The JetBrains Mono Project Authors
  (https://github.com/JetBrains/JetBrainsMono)
- Space Grotesk — Copyright 2020 The Space Grotesk Project Authors
  (https://github.com/floriankarsten/space-grotesk)

    This Font Software is licensed under the SIL Open Font License, Version 1.1.
    This license is available with a FAQ at: https://scripts.sil.org/OFL

    -----------------------------------------------------------
    SIL OPEN FONT LICENSE Version 1.1 - 26 February 2007
    -----------------------------------------------------------

    PREAMBLE
    The goals of the Open Font License (OFL) are to stimulate worldwide
    development of collaborative font projects, to support the font creation
    efforts of academic and linguistic communities, and to provide a free and
    open framework in which fonts may be shared and improved in partnership
    with others.

    The OFL allows the licensed fonts to be used, studied, modified and
    redistributed freely as long as they are not sold by themselves. The fonts,
    including any derivative works, can be bundled, embedded, redistributed
    and/or sold with any software provided that any reserved names are not used
    by derivative works. The fonts and derivatives, however, cannot be released
    under any other type of license. The requirement for fonts to remain under
    this license does not apply to any document created using the fonts or their
    derivatives.

    DEFINITIONS
    "Font Software" refers to the set of files released by the Copyright
    Holder(s) under this license and clearly marked as such. This may include
    source files, build scripts and documentation.

    "Reserved Font Name" refers to any names specified as such after the
    copyright statement(s).

    "Original Version" refers to the collection of Font Software components as
    distributed by the Copyright Holder(s).

    "Modified Version" refers to any derivative made by adding to, deleting, or
    substituting -- in part or in whole -- any of the components of the Original
    Version, by changing formats or by porting the Font Software to a new
    environment.

    "Author" refers to any designer, engineer, programmer, technical writer or
    other person who contributed to the Font Software.

    PERMISSION & CONDITIONS
    Permission is hereby granted, free of charge, to any person obtaining a copy
    of the Font Software, to use, study, copy, merge, embed, modify, redistribute,
    and sell modified and unmodified copies of the Font Software, subject to the
    following conditions:

    1) Neither the Font Software nor any of its individual components, in
    Original or Modified Versions, may be sold by itself.

    2) Original or Modified Versions of the Font Software may be bundled,
    redistributed and/or sold with any software, provided that each copy contains
    the above copyright notice and this license. These can be included either as
    stand-alone text files, human-readable headers or in the appropriate
    machine-readable metadata fields within text or binary files as long as those
    fields can be easily viewed by the user.

    3) No Modified Version of the Font Software may use the Reserved Font Name(s)
    unless explicit written permission is granted by the corresponding Copyright
    Holder. This restriction only applies to the primary font name as presented
    to the users.

    4) The name(s) of the Copyright Holder(s) or the Author(s) of the Font
    Software shall not be used to promote, endorse or advertise any Modified
    Version, except to acknowledge the contribution(s) of the Copyright Holder(s)
    and the Author(s) or with their explicit written permission.

    5) The Font Software, modified or unmodified, in part or in whole, must be
    distributed entirely under this license, and must not be distributed under
    any other license. The requirement for fonts to remain under this license
    does not apply to any document created using the Font Software.

    TERMINATION
    This license becomes null and void if any of the above conditions are not
    met.

    DISCLAIMER
    THE FONT SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS
    OR IMPLIED, INCLUDING BUT NOT LIMITED TO ANY WARRANTIES OF MERCHANTABILITY,
    FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT OF COPYRIGHT, PATENT,
    TRADEMARK, OR OTHER RIGHT. IN NO EVENT SHALL THE COPYRIGHT HOLDER BE LIABLE
    FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, INCLUDING ANY GENERAL, SPECIAL,
    INDIRECT, INCIDENTAL, OR CONSEQUENTIAL DAMAGES, WHETHER IN AN ACTION OF
    CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF THE USE OR INABILITY TO USE
    THE FONT SOFTWARE OR FROM OTHER DEALINGS IN THE FONT SOFTWARE.

### 2.3 MIT License (Angular, zone.js, xterm.js, and other MIT components)

    Permission is hereby granted, free of charge, to any person obtaining a copy
    of this software and associated documentation files (the "Software"), to deal
    in the Software without restriction, including without limitation the rights
    to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
    copies of the Software, and to permit persons to whom the Software is
    furnished to do so, subject to the following conditions:

    The above copyright notice and this permission notice shall be included in
    all copies or substantial portions of the Software.

    THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
    IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
    FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
    AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
    LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
    OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
    SOFTWARE.

### 2.4 0BSD — tslib

    Copyright (c) Microsoft Corporation.

    Permission to use, copy, modify, and/or distribute this software for any
    purpose with or without fee is hereby granted.

    THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
    REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY
    AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
    INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
    LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR
    OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
    PERFORMANCE OF THIS SOFTWARE.

### 2.5 Apache-2.0 — RxJS and other Apache-2.0 components

The Apache License 2.0 text is reproduced in the root `LICENSE` file; RxJS and the
other Apache-2.0 components listed above are covered by that same text.
