//! The orch intro: a ~37 second promo built from the landing page's story.
//!
//! Open (the mark and "Orchestration for the agentic era"), Cluster (three commands to a
//! cluster, then one file deployed to every server), Agents (an MCP client fixing an app
//! with a token scoped to it), Batteries (databases and notifications) and Outro.
//!
//! Colors, fonts and copy follow the dark theme of the website (`website/src/app.css`).
//! Every scene is drawn on a 1920x1080 grid. Check it with the CLI in `main.rs`.
use fframes::{
    AnimateRuntimeInput, AudioMap, AudioTimestamp::{Eof, Second}, AudioTrack, Color, Duration, FFramesContext, FontQuery, Frame, Overlap,
    Scene, Scenes, Svgr, Video,
    animation::{AnimationRuntime, Easing},
    include_media_dir,
};
use std::f32::consts::PI;
use std::sync::LazyLock;

// Every file of `media/` is embedded into the binary. Fonts are used by family name.
include_media_dir!(pub struct OrchIntroMedia, "media", stereo);

pub const WIDTH: usize = 1920;
pub const HEIGHT: usize = 1080;

const BG: &str = "#0b0c0c";
const FG: &str = "#ececec";
const DIM: &str = "#8a8f91";
const BOX: &str = "#121313";
const EDGE: &str = "#383a3a";
const ACCENT: &str = "#43b6a9";
const TINT: &str = "#132422";
const BAD: &str = "#d4473f";
const MARK_FILL: &str = "#111313";
const SANS: &str = "Geist";
const MONO: &str = "Geist Mono";
/// Advance of one Geist Mono glyph, in em.
const MONO_ADVANCE: f32 = 0.6;

static ENTER: LazyLock<AnimationRuntime> =
    LazyLock::new(|| AnimationRuntime::new(0.45, &Easing::EaseOut));
static LEAVE: LazyLock<AnimationRuntime> =
    LazyLock::new(|| AnimationRuntime::new(0.3, &Easing::EaseIn));
static MOVE: LazyLock<AnimationRuntime> =
    LazyLock::new(|| AnimationRuntime::new(0.7, &Easing::EaseInOut));
static PACKET: LazyLock<AnimationRuntime> =
    LazyLock::new(|| AnimationRuntime::new(0.45, &Easing::EaseInOut));
static DRAW: LazyLock<AnimationRuntime> =
    LazyLock::new(|| AnimationRuntime::new(0.45, &Easing::EaseInOut));
static FLASH: LazyLock<AnimationRuntime> =
    LazyLock::new(|| AnimationRuntime::new(0.9, &Easing::EaseOut));
/// Springs stop at their own settle time, 3 s is only the upper bound.
static SPRING: LazyLock<AnimationRuntime> = LazyLock::new(|| {
    AnimationRuntime::new(3.0, &Easing::Spring { mass: 1.0, stiffness: 210.0, damping: 20.0 })
});

fn tween(frame: &Frame, runtime: &AnimationRuntime, start: f32, from: f32, to: f32) -> f32 {
    frame.animate_runtime(AnimateRuntimeInput { on_second: start, from, to, animation_runtime: runtime })
}

/// Opacity 0 -> 1 from `start`.
fn appear(frame: &Frame, start: f32) -> f32 {
    tween(frame, &ENTER, start, 0.0, 1.0)
}

/// Opacity 1 -> 0 from `start`, faster than `appear`.
fn leave(frame: &Frame, start: f32) -> f32 {
    tween(frame, &LEAVE, start, 1.0, 0.0)
}

/// An offset that springs from `distance` to 0.
fn rise(frame: &Frame, start: f32, distance: f32) -> f32 {
    tween(frame, &SPRING, start, distance, 0.0)
}

/// A scale that springs from 0.9 to 1.
fn grow(frame: &Frame, start: f32) -> f32 {
    tween(frame, &SPRING, start, 0.9, 1.0)
}

/// 0 -> 1 for strokes and lines that draw on.
fn draw(frame: &Frame, start: f32) -> f32 {
    tween(frame, &DRAW, start, 0.0, 1.0)
}

/// 0 -> 1 for things that travel from one place to another.
fn travel(frame: &Frame, start: f32) -> f32 {
    tween(frame, &MOVE, start, 0.0, 1.0)
}

/// 1 -> 0 highlight after an event at `start`; 0 before it.
fn flash(frame: &Frame, start: f32) -> f32 {
    if frame.seconds() < start { 0.0 } else { tween(frame, &FLASH, start, 1.0, 0.0) }
}

fn lerp(a: f32, b: f32, t: f32) -> f32 {
    a + (b - a) * t
}

fn scale_at(cx: f32, cy: f32, scale: f32) -> String {
    format!("translate({cx} {cy}) scale({scale}) translate({} {})", -cx, -cy)
}

fn translate(x: f32, y: f32) -> String {
    format!("translate({x} {y})")
}

fn mono_width(chars: usize, size: f32) -> f32 {
    chars as f32 * size * MONO_ADVANCE
}

/// The first characters of `text`, typed at `cps` characters per second from `start`.
fn typed(frame: &Frame, text: &'static str, start: f32, cps: f32) -> &'static str {
    let count = ((frame.seconds() - start) * cps).floor().clamp(0.0, text.len() as f32) as usize;
    let end = text.char_indices().nth(count).map_or(text.len(), |(i, _)| i);
    &text[..end]
}

/// A block cursor at `x`, shown from `from` and blinking once typing is done at `done`,
/// gone at `until`.
fn cursor<'a>(frame: &Frame, x: f32, baseline: f32, size: f32, from: f32, done: f32, until: f32) -> Svgr<'a> {
    let t = frame.seconds();
    let on = t >= from && t < until && (t < done || ((t - done) / 0.5).floor() as i32 % 2 == 0);
    if !on {
        return Svgr::empty();
    }
    fframes::svgr!(<rect x={x + 2.0} y={baseline - size * 0.8} width={size * 0.5} height={size * 0.95} fill={ACCENT} opacity="0.85" />)
}

/// A shell line: a dim `$` prompt and a command typed after it.
fn shell_line<'a>(frame: &Frame, x: f32, baseline: f32, size: f32, command: &'static str, start: f32, cps: f32, cursor_until: f32) -> Svgr<'a> {
    let shown = start - 0.25;
    if frame.seconds() < shown {
        return Svgr::empty();
    }
    let text = typed(frame, command, start, cps);
    let done = start + command.chars().count() as f32 / cps;
    let text_x = x + mono_width(2, size);
    let cursor_x = text_x + mono_width(text.chars().count(), size);
    fframes::svgr!(<g font-family={MONO} font-size={size}>
        <text x={x} y={baseline} fill={DIM}>"$"</text>
        <text x={text_x} y={baseline} fill={FG}>{text}</text>
        {cursor(frame, cursor_x, baseline, size, shown, done, cursor_until)}
    </g>)
}

/// The orch mark from the favicon: a terminal prompt in a rounded square. `draw_from` animates
/// the chevron drawing on and the underscore growing; `None` shows the finished mark.
fn mark<'a>(frame: &Frame, x: f32, y: f32, size: f32, draw_from: Option<f32>, blink_from: f32) -> Svgr<'a> {
    let (chevron, underscore) = match draw_from {
        Some(start) => (draw(frame, start), tween(frame, &ENTER, start + 0.4, 0.0, 1.0)),
        None => (1.0, 1.0),
    };
    // Length of the chevron path in the 32 unit favicon grid.
    let length = 15.63;
    let t = frame.seconds();
    let blink = if t > blink_from && ((t - blink_from) / 0.55).floor() as i32 % 2 == 1 { 0.15 } else { 1.0 };
    let underscore_end = 17.0 + 7.0 * underscore;
    let transform = format!("translate({x} {y}) scale({})", size / 32.0);
    fframes::svgr!(<g transform={transform}>
        <rect x="0.15" y="0.15" width="31.7" height="31.7" rx="7" fill={MARK_FILL} stroke={EDGE} stroke-width="0.3" />
        <path d="M9 11l6 5-6 5" fill="none" stroke={ACCENT} stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"
            stroke-dasharray={length} stroke-dashoffset={length * (1.0 - chevron)} />
        {if underscore > 0.01 {
            fframes::svgr!(<path d={format!("M17 21H{underscore_end}")} stroke={FG} stroke-width="2.6" stroke-linecap="round" opacity={blink} />)
        } else {
            Svgr::empty()
        }}
    </g>)
}

/// A padlock in a ring, for encrypted links and scoped tokens.
fn lock<'a>(cx: f32, cy: f32, scale: f32, opacity: f32) -> Svgr<'a> {
    fframes::svgr!(<g transform={scale_at(cx, cy, scale)} opacity={opacity}>
        <circle cx={cx} cy={cy} r="19" fill={BG} stroke={ACCENT} stroke-width="1.5" />
        <path d={format!("M{} {}v-4a5 5 0 0 1 10 0v4", cx - 5.0, cy - 1.0)} fill="none" stroke={ACCENT} stroke-width="2" />
        <rect x={cx - 7.5} y={cy - 2.0} width="15" height="11" rx="2.5" fill={ACCENT} />
    </g>)
}

/// A small inline padlock without the ring.
fn small_lock<'a>(x: f32, y: f32, color: &'a str) -> Svgr<'a> {
    fframes::svgr!(<g>
        <path d={format!("M{} {}v-4a5 5 0 0 1 10 0v4", x + 2.5, y)} fill="none" stroke={color} stroke-width="2" />
        <rect x={x} y={y - 1.0} width="15" height="11" rx="2.5" fill={color} />
    </g>)
}

fn spinner<'a>(frame: &Frame, cx: f32, cy: f32) -> Svgr<'a> {
    let angle = frame.seconds() * 400.0;
    fframes::svgr!(<circle cx={cx} cy={cy} r="9" fill="none" stroke={ACCENT} stroke-width="3" stroke-linecap="round"
        stroke-dasharray="34 23" transform={format!("rotate({angle} {cx} {cy})")} />)
}

fn check<'a>(x: f32, y: f32, color: &'a str) -> Svgr<'a> {
    fframes::svgr!(<path d={format!("M{x} {y}l6 6 11-13")} fill="none" stroke={color} stroke-width="3" stroke-linecap="round" stroke-linejoin="round" />)
}

/// A dashed line that draws from (x1, y1) towards (x2, y2) from `start`.
fn wire<'a>(frame: &Frame, x1: f32, y1: f32, x2: f32, y2: f32, start: f32) -> Svgr<'a> {
    let p = draw(frame, start);
    if p < 0.01 {
        return Svgr::empty();
    }
    fframes::svgr!(<line x1={x1} y1={y1} x2={lerp(x1, x2, p)} y2={lerp(y1, y2, p)} stroke={EDGE} stroke-width="2" stroke-dasharray="6 8" />)
}

/// Endless traffic on a link: a dot that runs from (x1, y1) to (x2, y2) every `period` seconds.
fn traffic<'a>(frame: &Frame, x1: f32, y1: f32, x2: f32, y2: f32, from: f32, period: f32, phase: f32) -> Svgr<'a> {
    let t = frame.seconds() - from;
    if t < 0.0 {
        return Svgr::empty();
    }
    let u = (t / period + phase).fract();
    let opacity = (u * PI).sin() * appear(frame, from);
    fframes::svgr!(<circle cx={lerp(x1, x2, u)} cy={lerp(y1, y2, u)} r="5" fill={ACCENT} opacity={opacity} />)
}

/// A two-line headline in the left column.
fn headline<'a>(frame: &Frame, lines: [&'a str; 2], start: f32, leave_at: f32) -> Svgr<'a> {
    let rows: Vec<Svgr> = lines
        .into_iter()
        .enumerate()
        .map(|(i, line)| {
            let s = start + i as f32 * 0.08;
            let y = 250.0 + i as f32 * 92.0;
            let exit = leave(frame, leave_at + i as f32 * 0.04);
            let dy = rise(frame, s, 50.0) + (1.0 - exit) * -24.0;
            fframes::svgr!(<text x="160" y={y + dy} opacity={appear(frame, s) * exit} font-family={SANS} font-weight="600" font-size="80" letter-spacing="-2" fill={FG}>{line}</text>)
        })
        .collect();
    fframes::svgr!(<g>{rows}</g>)
}

fn panel<'a>(x: f32, y: f32, width: f32, height: f32) -> Svgr<'a> {
    fframes::svgr!(<rect x={x + 0.75} y={y + 0.75} width={width - 1.5} height={height - 1.5} rx="16" fill={BOX} stroke={EDGE} stroke-width="1.5" />)
}

/// A sound effect from `media/` at `at` seconds into the scene. Gains are relative to the
/// effects' common level, which sits a little above the music.
fn sfx(name: &'static str, at: f32, gain_db: f32) -> AudioTrack<'static> {
    AudioTrack::new(name, Second(at.max(0.0))..Eof).gain_db(gain_db + SFX_DB)
}

const SFX_DB: f32 = 2.0;

/// Key clicks for `chars` characters typed at `cps` from `start`. `take` picks a different
/// stretch of the clicks so lines do not sound the same.
fn typing(start: f32, chars: usize, cps: f32, take: f32) -> AudioTrack<'static> {
    let end = start + chars as f32 / cps;
    AudioTrack::new("sfx-type.mp3", Second(start)..Second(end)).offset(take).gain_db(-15.0 + SFX_DB)
}

pub struct OrchIntroVideo<'a> {
    pub media: &'a OrchIntroMedia,
}

impl<'a> OrchIntroVideo<'a> {
    pub fn new(media: &'a OrchIntroMedia) -> Self {
        Self { media }
    }
}

impl std::fmt::Debug for OrchIntroVideo<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("OrchIntroVideo").finish()
    }
}

impl Video for OrchIntroVideo<'_> {
    const FPS: usize = 60;
    const WIDTH: usize = WIDTH;
    const HEIGHT: usize = HEIGHT;
    const BACKGROUND_COLOR: Color = Color::BLACK;

    fn duration(&self) -> Duration<'_> {
        Duration::Auto
    }

    fn audio(&self) -> AudioMap<'_> {
        // Composed by `audio/compose.py` at 120 BPM; the scene cuts land on its beats.
        AudioMap::from([AudioTrack::new("music.mp3", Second(0.0)..Eof).gain_db(-5.0).fade_out(1.5)])
    }

    fn define_scenes(&self) -> Scenes<'_> {
        Scenes::from(vec![&Open as &dyn Scene, &Cluster, &Agents, &Batteries, &Outro])
    }

    fn render_frame<'a>(&'a self, frame: Frame, ctx: &FFramesContext<'a, '_>) -> Svgr<'a> {
        // A dot grid that drifts slowly and a soft glow keep the background alive.
        let t = frame.seconds();
        let grid = translate(-(t * 9.0) % 48.0, -(t * 5.0) % 48.0);
        let glow_x = 1450.0 + (t / 5.0).sin() * 140.0;
        let glow_y = 160.0 + (t / 7.0).cos() * 60.0;
        fframes::svgr!(
            <svg xmlns="http://www.w3.org/2000/svg" width={WIDTH} height={HEIGHT} viewBox="0 0 1920 1080">
                <defs>
                    <pattern id="dots" width="48" height="48" patternUnits="userSpaceOnUse">
                        <circle cx="24" cy="24" r="1.6" fill="#1b1d1d" />
                    </pattern>
                    <radialGradient id="glow">
                        <stop offset="0" stop-color={ACCENT} stop-opacity="0.11" />
                        <stop offset="1" stop-color={ACCENT} stop-opacity="0" />
                    </radialGradient>
                    <radialGradient id="vignette" cx="0.5" cy="0.5" r="0.75">
                        <stop offset="0.55" stop-color="#000" stop-opacity="0" />
                        <stop offset="1" stop-color="#000" stop-opacity="0.55" />
                    </radialGradient>
                </defs>
                <rect width="1920" height="1080" fill={BG} />
                <rect x="-48" y="-48" width="2064" height="1224" fill="url(#dots)" transform={grid} />
                <circle cx={glow_x} cy={glow_y} r="950" fill="url(#glow)" />
                <rect width="1920" height="1080" fill="url(#vignette)" />
                {ctx.render_scenes(&frame)}
            </svg>
        )
    }
}

// ---------------------------------------------------------------------------------------
// Open: the mark draws itself, makes room for the wordmark, and the kicker is typed.

#[derive(Debug)]
struct Open;

const OPEN_SECONDS: f32 = 3.65;
const KICKER: &str = "Orchestration for the agentic era";

impl Scene for Open {
    fn duration(&self) -> Duration<'_> {
        Duration::Seconds(OPEN_SECONDS)
    }

    fn audio(&self) -> AudioMap<'_> {
        AudioMap::from(vec![
            sfx("sfx-pop.mp3", 0.1, -13.0),
            sfx("sfx-whoosh.mp3", 0.85, -17.0),
            sfx("sfx-pop.mp3", 1.3, -14.0),
            typing(1.95, KICKER.len(), 38.0, 0.0),
        ])
    }

    fn render_frame<'a>(&'a self, mut frame: Frame, ctx: &FFramesContext<'a, '_>) -> Svgr<'a> {
        let size = 168.0;
        let gap = 44.0;
        let word_size = 150;
        let word_width = frame
            .text_width(ctx, FontQuery { family: SANS, size: word_size, weight: 600, ..Default::default() }, "orch")
            .unwrap_or(300) as f32;
        let group = size + gap + word_width;
        let shift = travel(&frame, 1.0);
        let mark_x = lerp(960.0 - size / 2.0, 960.0 - group / 2.0, shift);
        let mark_y = 470.0 - size / 2.0;
        let word_x = mark_x + size + gap;
        let word_in = appear(&frame, 1.3);
        let word_dx = rise(&frame, 1.3, -36.0);

        let kicker_size = 34.0;
        let kicker_x = 960.0 - mono_width(KICKER.len(), kicker_size) / 2.0;
        let kicker = typed(&frame, KICKER, 1.95, 38.0);
        let kicker_done = 1.95 + KICKER.len() as f32 / 38.0;

        let exit = leave(&frame, OPEN_SECONDS - 0.35);
        let lift = (1.0 - exit) * -30.0;
        let pop = grow(&frame, 0.1);

        fframes::svgr!(<g opacity={exit} transform={translate(0.0, lift)}>
            <g opacity={appear(&frame, 0.1)} transform={scale_at(mark_x + size / 2.0, 470.0, pop)}>
                {mark(&frame, mark_x, mark_y, size, Some(0.35), 1.2)}
            </g>
            <text x={word_x + word_dx} y="523" opacity={word_in} font-family={SANS} font-weight="600" font-size={word_size} letter-spacing="-3" fill={FG}>"orch"</text>
            <text x={kicker_x} y="680" font-family={MONO} font-size={kicker_size} fill={ACCENT}>{kicker}</text>
            {cursor(&frame, kicker_x + mono_width(kicker.len(), kicker_size), 680.0, kicker_size, 1.8, kicker_done, OPEN_SECONDS)}
        </g>)
    }
}

// ---------------------------------------------------------------------------------------
// Cluster: three commands build a cluster on the right, then one file is deployed to it.

#[derive(Debug)]
struct Cluster;

const CLUSTER_SECONDS: f32 = 12.15;
const DEPLOY_AT: f32 = 5.6;

struct Node {
    name: &'static str,
    role: &'static str,
    x: f32,
    y: f32,
    at: f32,
}

const NODE_W: f32 = 300.0;
const NODE_H: f32 = 170.0;
const NODES: [Node; 3] = [
    Node { name: "controller", role: "orch init", x: 1220.0, y: 270.0, at: 1.05 },
    Node { name: "worker-1", role: "orch join", x: 980.0, y: 660.0, at: 3.95 },
    Node { name: "worker-2", role: "orch join", x: 1460.0, y: 660.0, at: 4.12 },
];

const FLY_AT: f32 = 7.35;
const LAND_AFTER: f32 = 0.7;
const HEALTHY_AT: f32 = 8.75;
const INGRESS_AT: f32 = 9.7;

impl Cluster {
    fn node<'a>(&self, frame: &Frame, index: usize) -> Svgr<'a> {
        let n = &NODES[index];
        let t = frame.seconds();
        let cx = n.x + NODE_W / 2.0;
        let cy = n.y + NODE_H / 2.0;
        let landed = FLY_AT + index as f32 * 0.12 + LAND_AFTER;
        let healthy = HEALTHY_AT + index as f32 * 0.22;
        let status = if t < landed + 0.1 {
            Svgr::empty()
        } else if t < healthy {
            fframes::svgr!(<g opacity={appear(frame, landed + 0.1)}>
                {spinner(frame, n.x + 168.0, n.y + 126.0)}
                <text x={n.x + 188.0} y={n.y + 134.0} font-family={MONO} font-size="22" fill={DIM}>"checking"</text>
            </g>)
        } else {
            fframes::svgr!(<g>
                <circle cx={n.x + 168.0} cy={n.y + 126.0} r={tween(frame, &SPRING, healthy, 2.0, 6.0)} fill={ACCENT} />
                <text x={n.x + 188.0} y={n.y + 134.0} font-family={MONO} font-size="22" fill={FG}>"healthy"</text>
            </g>)
        };
        let glow = flash(frame, healthy);
        fframes::svgr!(<g opacity={appear(frame, n.at)} transform={scale_at(cx, cy, grow(frame, n.at))}>
            {panel(n.x, n.y, NODE_W, NODE_H)}
            <rect x={n.x + 0.75} y={n.y + 0.75} width={NODE_W - 1.5} height={NODE_H - 1.5} rx="16" fill="none" stroke={ACCENT} stroke-width="2" opacity={glow} />
            <text x={n.x + 24.0} y={n.y + 46.0} font-family={MONO} font-size="28" fill={FG}>{n.name}</text>
            <text x={n.x + 24.0} y={n.y + 80.0} font-family={MONO} font-size="22" fill={ACCENT}>{n.role}</text>
            {status}
        </g>)
    }

    /// The replica chips: they leave the `replicas = 3` line together and land in the nodes.
    fn replicas<'a>(&self, frame: &Frame) -> Vec<Svgr<'a>> {
        (0..3)
            .map(|i| {
                let start = FLY_AT + i as f32 * 0.12;
                if frame.seconds() < start - 0.15 {
                    return Svgr::empty();
                }
                let p = travel(frame, start);
                let (from_x, from_y) = (440.0, 603.0);
                let (to_x, to_y) = (NODES[i].x + 24.0, NODES[i].y + 104.0);
                let x = lerp(from_x, to_x, p);
                let y = lerp(from_y, to_y, p) - (p * PI).sin() * 110.0;
                let s = lerp(0.7, 1.0, p);
                fframes::svgr!(<g opacity={appear(frame, start - 0.15)} transform={format!("translate({x} {y}) scale({s})")}>
                    <rect x="0.75" y="0.75" width="110" height="44" rx="8" fill={TINT} stroke={ACCENT} stroke-width="1.5" />
                    <text x="56" y="30" text-anchor="middle" font-family={MONO} font-size="22" fill={ACCENT}>"api"</text>
                </g>)
            })
            .collect()
    }
}

impl Scene for Cluster {
    fn duration(&self) -> Duration<'_> {
        Duration::Seconds(CLUSTER_SECONDS)
    }

    fn audio(&self) -> AudioMap<'_> {
        let mut tracks = vec![
            typing(0.4, 14, 26.0, 0.0),
            typing(1.6, 22, 28.0, 0.9),
            typing(2.85, 28, 32.0, 1.8),
            typing(6.65, 11, 24.0, 2.7),
            sfx("sfx-whoosh.mp3", DEPLOY_AT, -19.0),
            sfx("sfx-whoosh.mp3", FLY_AT - 0.1, -13.0),
            sfx("sfx-pop.mp3", INGRESS_AT + 0.2, -13.0),
            sfx("sfx-lock.mp3", INGRESS_AT + 0.25, -15.0),
        ];
        for node in &NODES {
            tracks.push(sfx("sfx-pop.mp3", node.at, -12.0));
        }
        for at in [4.05, 4.22, 4.55] {
            tracks.push(sfx("sfx-lock.mp3", at + 0.4, -14.0));
        }
        for i in 0..3 {
            let i = i as f32;
            tracks.push(sfx("sfx-tick.mp3", FLY_AT + i * 0.12 + LAND_AFTER, -14.0));
            tracks.push(sfx("sfx-chime.mp3", HEALTHY_AT + i * 0.22, -16.0 + i));
        }
        AudioMap::from(tracks)
    }

    fn overlap(&self) -> Overlap {
        Overlap::Previous(0.15)
    }

    fn render_frame<'a>(&'a self, frame: Frame, _ctx: &FFramesContext<'a, '_>) -> Svgr<'a> {
        let f = &frame;
        let mid = |a: &Node| (a.x + NODE_W / 2.0, a.y);
        let (c, w1, w2) = (&NODES[0], &NODES[1], &NODES[2]);
        let (c_x, c_bottom) = (c.x + NODE_W / 2.0, c.y + NODE_H);
        let (w1_x, w1_top) = mid(w1);
        let (w2_x, w2_top) = mid(w2);
        let mesh_y = w1.y + NODE_H / 2.0;
        let links = [
            (c_x, c_bottom, w1_x, w1_top, 4.05),
            (c_x, c_bottom, w2_x, w2_top, 4.22),
            (w1.x + NODE_W, mesh_y, w2.x, mesh_y, 4.55),
        ];
        let wires: Vec<Svgr> = links.iter().map(|&(x1, y1, x2, y2, s)| wire(f, x1, y1, x2, y2, s)).collect();
        let locks: Vec<Svgr> = links
            .iter()
            .map(|&(x1, y1, x2, y2, s)| lock((x1 + x2) / 2.0, (y1 + y2) / 2.0, grow(f, s + 0.4), appear(f, s + 0.4)))
            .collect();
        let packets: Vec<Svgr> = links
            .iter()
            .enumerate()
            .flat_map(|(i, &(x1, y1, x2, y2, _))| {
                [traffic(f, x1, y1, x2, y2, 5.1, 1.6, i as f32 * 0.31), traffic(f, x2, y2, x1, y1, 5.1, 2.1, 0.5 + i as f32 * 0.2)]
            })
            .collect();
        let nodes: Vec<Svgr> = (0..3).map(|i| self.node(f, i)).collect();

        // The left column: a terminal for the cluster, then the app's file for the deploy.
        let terminal_out = leave(f, DEPLOY_AT);
        let terminal = fframes::svgr!(<g opacity={appear(f, 0.15) * terminal_out} transform={translate(0.0, rise(f, 0.15, 40.0) + (1.0 - terminal_out) * 30.0)}>
            {panel(160.0, 420.0, 700.0, 330.0)}
            {shell_line(f, 192.0, 490.0, 30.0, "sudo orch init", 0.4, 26.0, 1.4)}
            {shell_line(f, 192.0, 562.0, 30.0, "orch join-token worker", 1.6, 28.0, 2.6)}
            <text x="228" y="604" opacity={appear(f, 2.55)} font-family={MONO} font-size="26" fill={DIM}>"orch1.k7Qm9xT2…"</text>
            {shell_line(f, 192.0, 676.0, 30.0, "sudo orch join orch1.k7Qm9x…", 2.85, 32.0, DEPLOY_AT)}
            <text x="228" y="718" opacity={appear(f, 3.95)} font-family={MONO} font-size="22" fill={DIM}>"on each worker"</text>
        </g>);

        let code_at = DEPLOY_AT + 0.3;
        let code_lines: Vec<Svgr> = [("name", "\"api\""), ("image", "\"ghcr.io/acme/api:1.4\""), ("replicas", "3"), ("port", "8080")]
            .into_iter()
            .enumerate()
            .map(|(i, (key, value))| {
                let s = code_at + 0.15 + i as f32 * 0.07;
                let y = 545.0 + i as f32 * 46.0;
                // The replicas line lights up as the replicas leave it.
                let highlight = if i == 2 { flash(f, FLY_AT - 0.1) } else { 0.0 };
                fframes::svgr!(<g>
                <rect x="176" y={y - 32.0} width="668" height="44" rx="8" fill={TINT} opacity={highlight} />
                <text x="192" y={545.0 + i as f32 * 46.0} opacity={appear(f, s)} font-family={MONO} font-size="30" fill={FG}>
                    {key}<tspan fill={DIM}>" = "</tspan><tspan fill={ACCENT}>{value}</tspan>
                </text>
                </g>)
            })
            .collect();
        let deploy_end = CLUSTER_SECONDS - 0.35;
        let deploy_out = leave(f, deploy_end);
        let code = if f.seconds() < code_at {
            Svgr::empty()
        } else {
            fframes::svgr!(<g opacity={appear(f, code_at) * deploy_out} transform={translate(0.0, rise(f, code_at, 40.0))}>
                {panel(160.0, 420.0, 700.0, 330.0)}
                <text x="192" y="470" font-family={MONO} font-size="22" fill={DIM}>"orch.pkl"</text>
                <line x1="160" y1="494" x2="860" y2="494" stroke={EDGE} stroke-width="1.5" />
                {code_lines}
                {shell_line(f, 160.0, 830.0, 30.0, "orch deploy", 6.65, 24.0, FLY_AT + 0.4)}
            </g>)
        };

        // Ingress: the public URL arrives at the controller over HTTPS.
        let ingress_line = draw(f, INGRESS_AT);
        let pill_scale = grow(f, INGRESS_AT + 0.2);
        let ingress = if f.seconds() < INGRESS_AT {
            Svgr::empty()
        } else {
            fframes::svgr!(<g>
                <line x1={c_x} y1="206" x2={c_x} y2={lerp(206.0, c.y, ingress_line)} stroke={EDGE} stroke-width="2" stroke-dasharray="6 8" />
                {traffic(f, c_x, 206.0, c_x, c.y, INGRESS_AT + 0.6, 0.9, 0.0)}
                <g opacity={appear(f, INGRESS_AT + 0.2)} transform={scale_at(c_x, 178.0, pill_scale)}>
                    <rect x={c_x - 240.0} y="150.75" width="480" height="56" rx="28" fill={BG} stroke={EDGE} stroke-width="1.5" />
                    {small_lock(c_x - 200.0, 179.0, ACCENT)}
                    <text x={c_x - 172.0} y="188" font-family={MONO} font-size="26" fill={FG}>"https://api.example.com"</text>
                </g>
            </g>)
        };

        fframes::svgr!(<g>
            {headline(f, ["A cluster in", "three commands."], 0.05, DEPLOY_AT - 0.1)}
            {if f.seconds() > DEPLOY_AT { headline(f, ["One file,", "every server."], DEPLOY_AT + 0.2, deploy_end) } else { Svgr::empty() }}
            {terminal}
            {code}
            <g opacity={deploy_out}>
                {wires}
                <text x={(w1.x + NODE_W + w2.x) / 2.0} y={mesh_y + 62.0} opacity={appear(f, 5.0)} text-anchor="middle" font-family={MONO} font-size="22" fill={DIM}>"wireguard"</text>
                {packets}
                {locks}
                {ingress}
                {nodes}
                {self.replicas(f)}
            </g>
        </g>)
    }
}

// ---------------------------------------------------------------------------------------
// Agents: an MCP client finds and fixes a broken app, with a token that only reaches it.

#[derive(Debug)]
struct Agents;

const AGENTS_SECONDS: f32 = 8.0;
const PROMPT: &str = "why is shop not ready?";
/// When each tool call appears: app_status, logs, deploy.
const CALLS: [f32; 3] = [1.6, 2.4, 3.3];
const FIXED_AT: f32 = 4.3;
const SCOPE_AT: f32 = 0.95;

impl Scene for Agents {
    fn duration(&self) -> Duration<'_> {
        Duration::Seconds(AGENTS_SECONDS)
    }

    fn audio(&self) -> AudioMap<'_> {
        let mut tracks = vec![
            typing(0.55, PROMPT.len(), 30.0, 0.4),
            sfx("sfx-lock.mp3", SCOPE_AT, -13.0),
            sfx("sfx-error.mp3", CALLS[1] + 0.55, -15.0),
            sfx("sfx-whoosh.mp3", CALLS[2] + 0.05, -15.0),
            sfx("sfx-chime.mp3", FIXED_AT, -12.0),
            sfx("sfx-pop.mp3", FIXED_AT + 0.3, -15.0),
        ];
        for (i, &at) in CALLS.iter().enumerate() {
            tracks.push(sfx("sfx-pop.mp3", at, -16.0));
            tracks.push(sfx("sfx-tick.mp3", at + 0.5, -15.0));
            if i < 2 {
                tracks.push(sfx("sfx-tick.mp3", at + 0.9, -17.0));
            }
        }
        AudioMap::from(tracks)
    }

    fn overlap(&self) -> Overlap {
        Overlap::Previous(0.3)
    }

    fn render_frame<'a>(&'a self, frame: Frame, _ctx: &FFramesContext<'a, '_>) -> Svgr<'a> {
        let f = &frame;
        let t = f.seconds();
        let exit = leave(f, AGENTS_SECONDS - 0.35);
        let prompt = typed(f, PROMPT, 0.55, 30.0);
        let prompt_done = 0.55 + PROMPT.len() as f32 / 30.0;

        let rows: Vec<Svgr> = [("app_status", "shop", "read"), ("logs", "shop", "read"), ("deploy", "orch.pkl", "write")]
            .into_iter()
            .enumerate()
            .map(|(i, (tool, arg, kind))| {
                let at = CALLS[i];
                let y = 470.0 + i as f32 * 82.0;
                let write = kind == "write";
                let running = t >= at && t < at + 0.75;
                let r = if running { 6.0 + ((t - at) * 12.0).sin().abs() * 3.0 } else { 6.0 };
                let (pill_fill, pill_stroke, pill_text) = if write { (TINT, ACCENT, ACCENT) } else { (BG, EDGE, DIM) };
                fframes::svgr!(<g opacity={appear(f, at)} transform={translate(rise(f, at, -30.0), 0.0)}>
                    {panel(160.0, y, 700.0, 66.0)}
                    <circle cx="194" cy={y + 33.0} r={r} fill={ACCENT} />
                    <text x="220" y={y + 43.0} font-family={MONO} font-size="28" fill={FG}>{tool}<tspan fill={DIM}>" "{arg}</tspan></text>
                    <rect x="720.75" y={y + 17.75} width="110" height="32" rx="16" fill={pill_fill} stroke={pill_stroke} stroke-width="1.5" />
                    <text x="776" y={y + 41.0} text-anchor="middle" font-family={MONO} font-size="20" fill={pill_text}>{kind}</text>
                </g>)
            })
            .collect();

        // Calls travel from the transcript to the app: reads come back with data, the write
        // does not.
        let scope_left = 1082.0;
        let target_y = 420.0;
        let calls: Vec<Svgr> = CALLS
            .iter()
            .enumerate()
            .map(|(i, &at)| {
                let (x1, y1) = (860.0, 503.0 + i as f32 * 82.0);
                let out = tween(f, &PACKET, at + 0.1, 0.0, 1.0);
                let back = tween(f, &PACKET, at + 0.5, 0.0, 1.0);
                let write = i == 2;
                let (u, visible) = if write || t < at + 0.5 { (out, t > at + 0.1 && out < 1.0) } else { (1.0 - back, back < 1.0) };
                let dot = if visible {
                    fframes::svgr!(<circle cx={lerp(x1, scope_left, u)} cy={lerp(y1, target_y, u)} r={if write { 8.0 } else { 6.0 }} fill={ACCENT} />)
                } else {
                    Svgr::empty()
                };
                fframes::svgr!(<g>
                    {wire(f, x1, y1, scope_left, target_y, at)}
                    {dot}
                </g>)
            })
            .collect();

        let reply_at = FIXED_AT + 0.3;
        let reply = fframes::svgr!(<g opacity={appear(f, reply_at)} transform={translate(0.0, rise(f, reply_at, 30.0))}>
            <rect x="160.75" y="720.75" width="698.5" height="110.5" rx="16" fill={TINT} stroke={ACCENT} stroke-width="1.5" />
            <text x="192" y="766" font-family={MONO} font-size="28" fill={FG}>"Set the missing DATABASE_URL"</text>
            <text x="192" y="808" font-family={MONO} font-size="28" fill={ACCENT}>"and redeployed. shop is healthy."</text>
        </g>);

        let out_of_scope = lerp(1.0, 0.38, appear(f, SCOPE_AT + 0.15));
        let apps: Vec<Svgr> = ["shop", "docs", "api"]
            .into_iter()
            .enumerate()
            .map(|(i, name)| {
                let y = 360.0 + i as f32 * 150.0;
                let s = 0.25 + i as f32 * 0.08;
                let shop = i == 0;
                let status = if !shop || t >= FIXED_AT {
                    let healthy_dot = if shop { tween(f, &SPRING, FIXED_AT, 2.0, 7.0) } else { 7.0 };
                    fframes::svgr!(<g>
                        <circle cx="1586" cy={y + 44.0} r={healthy_dot} fill={ACCENT} />
                        <text x="1724" y={y + 52.0} text-anchor="end" font-family={MONO} font-size="24" fill={FG}>"healthy"</text>
                    </g>)
                } else if t >= CALLS[2] + 0.3 {
                    fframes::svgr!(<g>
                        {spinner(f, 1560.0, y + 44.0)}
                        <text x="1724" y={y + 52.0} text-anchor="end" font-family={MONO} font-size="24" fill={DIM}>"deploying"</text>
                    </g>)
                } else {
                    fframes::svgr!(<g>
                        <circle cx="1560" cy={y + 44.0} r="7" fill={BAD} />
                        <text x="1724" y={y + 52.0} text-anchor="end" font-family={MONO} font-size="24" fill={BAD}>"not ready"</text>
                    </g>)
                };
                let log = if shop {
                    let o = appear(f, CALLS[1] + 0.55) * leave(f, FIXED_AT);
                    fframes::svgr!(<g>
                        <text x="1136" y={y + 94.0} opacity={o} font-family={MONO} font-size="22" fill={BAD}>"DATABASE_URL is not set"</text>
                        <text x="1136" y={y + 94.0} opacity={appear(f, FIXED_AT + 0.2)} font-family={MONO} font-size="22" fill={DIM}>"2 replicas"</text>
                    </g>)
                } else {
                    fframes::svgr!(<text x="1136" y={y + 94.0} font-family={MONO} font-size="22" fill={DIM}>"2 replicas"</text>)
                };
                let glow = if shop { flash(f, FIXED_AT) } else { 0.0 };
                let dim = if shop { 1.0 } else { out_of_scope };
                fframes::svgr!(<g opacity={appear(f, s) * dim} transform={translate(0.0, rise(f, s, 40.0))}>
                    {panel(1100.0, y, 660.0, 120.0)}
                    <rect x="1100.75" y={y + 0.75} width="658.5" height="118.5" rx="16" fill="none" stroke={ACCENT} stroke-width="2" opacity={glow} />
                    <text x="1136" y={y + 54.0} font-family={MONO} font-size="32" fill={FG}>{name}</text>
                    {status}
                    {log}
                </g>)
            })
            .collect();

        // The token's scope: a dashed boundary around shop only.
        let scope_scale = tween(f, &SPRING, SCOPE_AT, 1.04, 1.0);
        let scope = fframes::svgr!(<g opacity={appear(f, SCOPE_AT)} transform={scale_at(1430.0, 420.0, scope_scale)}>
            <rect x="1082" y="342" width="696" height="156" rx="22" fill="none" stroke={ACCENT} stroke-width="2" stroke-dasharray="10 8" />
            <rect x="1104" y="324" width="300" height="36" rx="8" fill={BG} />
            {small_lock(1116.0, 343.0, ACCENT)}
            <text x="1142" y="350" font-family={MONO} font-size="22" fill={ACCENT}>"ORCH_TOKEN · shop"</text>
        </g>);

        fframes::svgr!(<g opacity={exit}>
            <text x="160" y={250.0 + rise(f, 0.05, 50.0)} opacity={appear(f, 0.05)} font-family={SANS} font-weight="600" font-size="80" letter-spacing="-2" fill={FG}>"Built for agents."</text>
            <text x="162" y={312.0 + rise(f, 0.15, 40.0)} opacity={appear(f, 0.15)} font-family={MONO} font-size="26" fill={DIM}>"MCP server for Claude Code, Cursor and Codex"</text>
            <g opacity={appear(f, 0.3)}>
                <rect x="160.75" y="380.75" width="698.5" height="62.5" rx="31" fill={BG} stroke={EDGE} stroke-width="1.5" />
                <text x="196" y="422" font-family={MONO} font-size="28" fill={FG}>{prompt}</text>
                {cursor(f, 196.0 + mono_width(prompt.len(), 28.0), 422.0, 28.0, 0.3, prompt_done, CALLS[0])}
            </g>
            {calls}
            {rows}
            {reply}
            {apps}
            {scope}
        </g>)
    }
}

// ---------------------------------------------------------------------------------------
// Batteries: a database wired into a service, and a failed deploy reported everywhere.

#[derive(Debug)]
struct Batteries;

const BATTERIES_SECONDS: f32 = 7.0;
const DB_LINE: &str = "databases { [\"main\"] = new Postgres {} }";
const DB_AT: f32 = 1.55;
const URL_AT: f32 = 2.15;
const RIGHT_AT: f32 = 2.9;
const FAILED_AT: f32 = 3.95;
const SEND_AT: f32 = 4.15;

impl Batteries {
    fn databases<'a>(&self, f: &Frame) -> Svgr<'a> {
        let t = f.seconds();
        let line = typed(f, DB_LINE, 0.5, 40.0);
        let done = 0.5 + DB_LINE.len() as f32 / 40.0;
        let arrived = URL_AT + 0.7;
        let url_color = if t >= arrived { ACCENT } else { DIM };
        let url_x = lerp(620.0, 400.0, travel(f, URL_AT));
        let url_pill = if t >= URL_AT - 0.2 && t < arrived + 0.3 {
            fframes::svgr!(<g opacity={appear(f, URL_AT - 0.2) * leave(f, arrived)} transform={translate(url_x, 580.0)}>
                <rect x="-66" y="-19" width="132" height="38" rx="19" fill={TINT} stroke={ACCENT} stroke-width="1.5" />
                <text x="0" y="7" text-anchor="middle" font-family={MONO} font-size="20" fill={ACCENT}>"main-url"</text>
            </g>)
        } else {
            Svgr::empty()
        };
        let backups: Vec<Svgr> = (0..3)
            .map(|i| {
                let s = 3.25 + i as f32 * 0.3;
                let x = 740.0 + i as f32 * 36.0;
                fframes::svgr!(<rect x={x} y="752" width="24" height="24" rx="5" fill={TINT} stroke={ACCENT} stroke-width="1.5"
                    opacity={appear(f, s)} transform={scale_at(x + 12.0, 764.0, tween(f, &SPRING, s, 0.4, 1.0))} />)
            })
            .collect();
        fframes::svgr!(<g>
            <text x="160" y={240.0 + rise(f, 0.05, 50.0)} opacity={appear(f, 0.05)} font-family={SANS} font-weight="600" font-size="68" letter-spacing="-2" fill={FG}>"Databases"</text>
            <text x="162" y={296.0 + rise(f, 0.15, 40.0)} opacity={appear(f, 0.15)} font-family={MONO} font-size="24" fill={DIM}>"Postgres, MySQL, Redis and more"</text>
            <g opacity={appear(f, 0.3)}>
                <rect x="160.75" y="360.75" width="738.5" height="60.5" rx="10" fill={BOX} stroke={EDGE} stroke-width="1.5" />
                <text x="186" y="400" font-family={MONO} font-size="26" fill={FG}>{line}</text>
                {cursor(f, 186.0 + mono_width(line.chars().count(), 26.0), 400.0, 26.0, 0.3, done, DB_AT)}
            </g>
            <g opacity={appear(f, 0.6)} transform={translate(0.0, rise(f, 0.6, 40.0))}>
                {panel(160.0, 520.0, 240.0, 120.0)}
                <text x="186" y="566" font-family={MONO} font-size="30" fill={FG}>"web"</text>
                <text x="186" y="608" font-family={MONO} font-size="22" fill={url_color}>"DATABASE_URL"</text>
                {if t >= arrived { fframes::svgr!(<g opacity={appear(f, arrived)}>{check(352.0, 598.0, ACCENT)}</g>) } else { Svgr::empty() }}
            </g>
            {wire(f, 400.0, 580.0, 620.0, 580.0, DB_AT + 0.4)}
            <g opacity={appear(f, DB_AT)} transform={scale_at(760.0, 585.0, grow(f, DB_AT))}>
                <path d="M620.75 500v170a139.25 26 0 0 0 278.5 0v-170" fill={BOX} stroke={EDGE} stroke-width="1.5" />
                <ellipse cx="760" cy="500" rx="139.25" ry="26" fill={BOX} stroke={EDGE} stroke-width="1.5" />
                <text x="760" y="596" text-anchor="middle" font-family={MONO} font-size="30" fill={FG}>"main"</text>
                <text x="760" y="634" text-anchor="middle" font-family={MONO} font-size="22" fill={DIM}>"postgres 17"</text>
            </g>
            {url_pill}
            <text x="620" y="771" opacity={appear(f, 3.1)} font-family={MONO} font-size="22" fill={DIM}>"backups"</text>
            {backups}
        </g>)
    }

    fn notifications<'a>(&self, f: &Frame) -> Svgr<'a> {
        let t = f.seconds();
        let x = 1020.0;
        let shake = if t >= FAILED_AT && t < FAILED_AT + 0.45 {
            ((t - FAILED_AT) * 55.0).sin() * 7.0 * (1.0 - (t - FAILED_AT) / 0.45)
        } else {
            0.0
        };
        let status = if t < FAILED_AT {
            fframes::svgr!(<g>
                {spinner(f, x + 36.0, 599.0)}
                <text x={x + 56.0} y="607" font-family={MONO} font-size="22" fill={DIM}>"deploying"</text>
            </g>)
        } else {
            fframes::svgr!(<text x={x + 26.0} y="607" font-family={MONO} font-size="22" fill={BAD}>"deploy failed"</text>)
        };
        let targets: Vec<Svgr> = ["Slack", "Discord", "Webhook"]
            .into_iter()
            .enumerate()
            .map(|(i, name)| {
                let y = 380.0 + i as f32 * 152.0;
                let s = RIGHT_AT + 0.25 + i as f32 * 0.08;
                let send = SEND_AT + i as f32 * 0.12;
                let arrive = send + 0.45;
                let p = tween(f, &PACKET, send, 0.0, 1.0);
                let (x1, y1, x2, y2) = (1280.0, 580.0, 1500.0, y + 48.0);
                let packet = if t > send && p < 1.0 {
                    fframes::svgr!(<circle cx={lerp(x1, x2, p)} cy={lerp(y1, y2, p)} r="7" fill={BAD} />)
                } else {
                    Svgr::empty()
                };
                let bar = [132.0, 104.0, 148.0][i];
                fframes::svgr!(<g>
                    {wire(f, x1, y1, x2, y2, s)}
                    {packet}
                    <g opacity={appear(f, s)} transform={translate(rise(f, s, 40.0), 0.0)}>
                        {panel(1500.0, y, 260.0, 96.0)}
                        <rect x="1500.75" y={y + 0.75} width="258.5" height="94.5" rx="16" fill="none" stroke={BAD} stroke-width="2" opacity={flash(f, arrive)} />
                        <text x="1526" y={y + 40.0} font-family={MONO} font-size="26" fill={FG}>{name}</text>
                        <g opacity={appear(f, arrive)} transform={translate(tween(f, &SPRING, arrive, -12.0, 0.0), 0.0)}>
                            <rect x="1526" y={y + 56.0} width="8" height="16" rx="2" fill={BAD} />
                            <rect x="1542" y={y + 58.0} width={bar} height="12" rx="4" fill={EDGE} />
                        </g>
                    </g>
                </g>)
            })
            .collect();
        fframes::svgr!(<g>
            <text x={x} y={240.0 + rise(f, RIGHT_AT, 50.0)} opacity={appear(f, RIGHT_AT)} font-family={SANS} font-weight="600" font-size="68" letter-spacing="-2" fill={FG}>"Notifications"</text>
            <text x={x + 2.0} y={296.0 + rise(f, RIGHT_AT + 0.1, 40.0)} opacity={appear(f, RIGHT_AT + 0.1)} font-family={MONO} font-size="24" fill={DIM}>"Slack, Discord or any webhook"</text>
            {targets}
            <g opacity={appear(f, RIGHT_AT + 0.15)} transform={translate(shake, rise(f, RIGHT_AT + 0.15, 40.0))}>
                {panel(x, 520.0, 260.0, 120.0)}
                <rect x={x + 0.75} y="520.75" width="258.5" height="118.5" rx="16" fill="none" stroke={BAD} stroke-width="2" opacity={flash(f, FAILED_AT)} />
                <text x={x + 26.0} y="566" font-family={MONO} font-size="30" fill={FG}>"shop"</text>
                {status}
            </g>
        </g>)
    }
}

impl Scene for Batteries {
    fn duration(&self) -> Duration<'_> {
        Duration::Seconds(BATTERIES_SECONDS)
    }

    fn audio(&self) -> AudioMap<'_> {
        let mut tracks = vec![
            typing(0.5, DB_LINE.len(), 40.0, 1.3),
            sfx("sfx-pop.mp3", DB_AT, -12.0),
            sfx("sfx-whoosh.mp3", URL_AT, -16.0),
            sfx("sfx-chime.mp3", URL_AT + 0.7, -14.0),
            sfx("sfx-whoosh.mp3", RIGHT_AT - 0.1, -20.0),
            sfx("sfx-error.mp3", FAILED_AT, -12.0),
        ];
        for i in 0..3 {
            let i = i as f32;
            tracks.push(sfx("sfx-tick.mp3", 3.25 + i * 0.3, -15.0));
            tracks.push(sfx("sfx-ping.mp3", SEND_AT + i * 0.12 + 0.45, -14.0).pan(0.4));
        }
        AudioMap::from(tracks)
    }

    fn overlap(&self) -> Overlap {
        Overlap::Previous(0.3)
    }

    fn render_frame<'a>(&'a self, frame: Frame, _ctx: &FFramesContext<'a, '_>) -> Svgr<'a> {
        let f = &frame;
        fframes::svgr!(<g opacity={leave(f, BATTERIES_SECONDS - 0.35)}>
            {self.databases(f)}
            <line x1="960" y1="200" x2="960" y2="860" stroke="#1b1d1d" stroke-width="2" opacity={appear(f, RIGHT_AT)} />
            {self.notifications(f)}
        </g>)
    }
}

// ---------------------------------------------------------------------------------------
// Outro: the promise from the hero, word by word, and where to find orch.

#[derive(Debug)]
struct Outro;

const OUTRO_SECONDS: f32 = 4.5;
const PROMISE: [&str; 5] = ["Deploy", "to", "your", "own", "servers."];

impl Scene for Outro {
    fn duration(&self) -> Duration<'_> {
        Duration::Seconds(OUTRO_SECONDS)
    }

    fn audio(&self) -> AudioMap<'_> {
        AudioMap::from([sfx("sfx-pop.mp3", 1.6, -15.0)])
    }

    fn overlap(&self) -> Overlap {
        Overlap::Previous(0.3)
    }

    fn render_frame<'a>(&'a self, mut frame: Frame, ctx: &FFramesContext<'a, '_>) -> Svgr<'a> {
        let size = 112;
        let font = FontQuery { family: SANS, size, weight: 600, ..Default::default() };
        let space = frame.text_width(ctx, font, "a b").unwrap_or(120) as f32
            - frame.text_width(ctx, font, "ab").unwrap_or(100) as f32;
        let widths: Vec<f32> = PROMISE.iter().map(|w| frame.text_width(ctx, font, w).unwrap_or(200) as f32).collect();
        let total = widths.iter().sum::<f32>() + space * (PROMISE.len() - 1) as f32;
        let mut x = 960.0 - total / 2.0;
        let f = &frame;
        let words: Vec<Svgr> = PROMISE
            .iter()
            .zip(&widths)
            .enumerate()
            .map(|(i, (word, width))| {
                let s = 0.45 + i as f32 * 0.07;
                let word_x = x;
                x += width + space;
                let fill = if i == PROMISE.len() - 1 { ACCENT } else { FG };
                fframes::svgr!(<text x={word_x} y={575.0 + rise(f, s, 60.0)} opacity={appear(f, s)} font-family={SANS} font-weight="600" font-size={size} fill={fill}>{*word}</text>)
            })
            .collect();

        let mark_size = 84.0;
        let word_size = 72;
        let word_w = frame
            .text_width(ctx, FontQuery { family: SANS, size: word_size, weight: 600, ..Default::default() }, "orch")
            .unwrap_or(150) as f32;
        let f = &frame;
        let group = mark_size + 26.0 + word_w;
        let mark_x = 960.0 - group / 2.0;
        let url = "orch.torsten.work";
        let url_w = mono_width(url.len(), 30.0) + 72.0;

        fframes::svgr!(<g>
            <g opacity={appear(f, 0.1)} transform={scale_at(960.0, 330.0, grow(f, 0.1))}>
                {mark(f, mark_x, 330.0 - mark_size / 2.0, mark_size, None, 1.0)}
                <text x={mark_x + mark_size + 26.0} y="356" font-family={SANS} font-weight="600" font-size={word_size} letter-spacing="-2" fill={FG}>"orch"</text>
            </g>
            {words}
            <text x="960" y={662.0 + rise(f, 1.1, 30.0)} opacity={appear(f, 1.1)} text-anchor="middle" font-family={SANS} font-weight="400" font-size="38" fill={DIM}>"The simplicity of Docker Swarm with the modern features of Kubernetes."</text>
            <g opacity={appear(f, 1.6)} transform={scale_at(960.0, 790.0, grow(f, 1.6))}>
                <rect x={960.0 - url_w / 2.0} y="758.75" width={url_w} height="62.5" rx="31" fill={TINT} stroke={ACCENT} stroke-width="1.5" />
                <text x="960" y="800" text-anchor="middle" font-family={MONO} font-size="30" fill={ACCENT}>{url}</text>
            </g>
        </g>)
    }
}
