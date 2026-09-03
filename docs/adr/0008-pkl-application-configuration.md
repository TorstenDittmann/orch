# ADR 0008: use Pkl for typed, composable application configuration

Status: Accepted by Plan #6; amended by ADR 0009.

## Decision

Applications are authored as Pkl modules that amend the versioned
`orch:/v1alpha1/Application.pkl` schema embedded in the `orch` binary. Pkl is
the only source format; strict TOML is removed in a pre-1.0 hard cut.

Pkl evaluation is an operator-side concern. `orch deploy`, `validate`, `plan`,
and `render` evaluate local modules, convert the result to the existing Go
`Manifest`, apply authoritative Go validation and normalization, and derive the
existing canonical JSON and digest. Only that canonical JSON crosses the apply
API. Controllers and agents do not execute Pkl and do not require it on PATH.

The evaluator allows standard-library modules, local file modules inside a
real-path root, and the embedded `orch:` module reader. It denies all resources
and all network, package, project-package, module-path, REPL, and arbitrary
external module schemes. User settings, environment variables, external
properties, package caches, and `PklProject` evaluator settings are not
inherited. Evaluation is deadline-bound and size-bound.

Pkl 0.32.1 is pinned in the repository. The Go SDK is pinned to Apple's
post-0.14.0 dispatcher fix (`a15d198922e0`) until that fix is included in a
tagged release; the 0.14.0 release can report cancellation as a successful nil
evaluator and deadlock on late responses.
The operator installs Pkl through mise. Bundling the executable is a release
packaging concern outside this decision.

The resolved `v1alpha1` model and snake_case canonical JSON do not change.
Pkl-facing properties use idiomatic camelCase. Existing stored revisions and
assignments therefore require no database migration, and equivalent Pkl source
produces the same digest as the former TOML source.

## Consequences

- Authors get a closed schema, type constraints, defaults, local amendments,
  functions, and reusable modules instead of copying configuration fragments.
- Review and planning remain semantic because source composition disappears at
  the canonical boundary.
- The Pkl executable is required for operator authoring commands and must match
  the pinned version for reproducible evaluation.
- CLI and controller must be upgraded together for the apply transport change.
  Old clients send the deprecated TOML field; new clients send canonical JSON.
  Both mixed-version directions fail closed, while agents are unaffected.
- The controller still validates untrusted canonical input. Pkl types are not
  treated as an authorization or network trust boundary.
- Stock Pkl tools may resolve the embedded schema through the `orch pkl reader`
  external-reader command. Public package and editor distribution remain future
  work.

## Alternatives considered

- Keep TOML and add a template preprocessor: this creates two interacting
  languages without giving the application model native types or composition.
- Run Pkl on the controller: this would upload an import graph, expand server
  I/O and denial-of-service exposure, and make desired state depend on remote
  evaluation inputs.
- Permit remote Pkl packages or environment/file resources: convenient, but it
  weakens reproducibility and risks leaking host or secret data into desired
  state. Local modules cover the initial composition need.
- Generate Go DTOs from Pkl annotations: the generator annotation introduces a
  package dependency into the runtime schema. Handwritten decode DTOs plus
  parity tests keep runtime evaluation self-contained for this version.
- Maintain a dual TOML/Pkl window or automated converter: unnecessary for the
  pre-1.0 repository and likely to preserve two subtly different defaulting
  paths. Migration guidance and golden digest comparison are sufficient.
