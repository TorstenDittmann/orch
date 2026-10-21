use fframes::{EncoderOptions, RenderOptions, StaticMediaProvider, cli};
use orch_intro::{OrchIntroMedia, OrchIntroVideo};
use std::process::ExitCode;

fn main() -> ExitCode {
    let media = OrchIntroMedia::prepare().expect("media");
    let video = OrchIntroVideo::new(&media);

    #[cfg(feature = "metal")]
    let gpu = fframes_skia_renderer::metal::SkiaMetalCtx::new(orch_intro::WIDTH, orch_intro::HEIGHT)
        .expect("GPU context");

    let cli = cli::new(
        &video,
        RenderOptions {
            media: Some(&media),
            video_encoder_options: EncoderOptions {
                preferred_encoder: Some("libx264"),
                codec_params: Some(&[("crf", "18"), ("preset", "slow"), ("tune", "animation")]),
                ..Default::default()
            },
            ..Default::default()
        },
    );

    // With `--features metal` frames render on the GPU and `preview` opens the real-time player.
    #[cfg(feature = "metal")]
    let cli = {
        use fframes_skia_renderer::{SkiaFFramesRenderer, SkiaPipelineConcurrencyPolicy, SkiaPipelineConfig};
        let config = SkiaPipelineConfig {
            concurrency_policy: SkiaPipelineConcurrencyPolicy::MaxPerformance,
            ..Default::default()
        };
        cli.backend(SkiaFFramesRenderer::new_metal(&gpu, config).expect("skia renderer"))
            .preview(fframes_native_player::cli_preview)
    };

    cli.run()
}
