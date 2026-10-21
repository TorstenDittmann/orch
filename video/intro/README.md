# orch intro video

A 1920x1080, 60 fps promo for orch, written in Rust with
[fframes](https://fframes.studio). It follows the landing page: the mark and
"Orchestration for the agentic era", a cluster in three commands, one file
deployed to every server, an agent fixing an app through the MCP server with a
scoped token, databases and notifications, and the hero line as the outro.

| file | what |
| --- | --- |
| `src/lib.rs` | the video: one struct per scene, colors and fonts from `website/src/app.css` |
| `src/main.rs` | the command line (`fframes::cli`) |
| `media/` | Geist and Geist Mono (SIL Open Font License, see `OFL.txt`) |

## Work on it

Install Rust and the libraries ffmpeg is built with (`brew install pkg-config ffmpeg x264 nasm`
on macOS, see the [fframes skill](https://github.com/dmtrKovalenko/fframes/tree/main/skills/fframes-video)
for Linux). On macOS add `--features metal` to every command to render on the GPU and
get the real-time `preview` window.

```sh
cargo run --release -- timeline                 # scenes and their seconds
cargo run --release -- strip Agents -n 12       # contact sheet of one scene, writes strip.png
cargo run --release -- frame Cluster@9s         # full-size PNG into frames/
cargo run --release -- inspect --pops           # clipped text, missing fonts, elements that pop
cargo run --release -- render -o orch-intro.mp4 # the final video
cargo run --release --features metal -- preview # macOS: real-time player
```

On Ubuntu 24.04 the prebuilt ffmpeg does not link against the system x264;
build it from source once with `FFMPEG_FORCE_BUILD=1 cargo build --release`.
