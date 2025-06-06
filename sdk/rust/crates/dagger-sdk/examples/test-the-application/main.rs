use dagger_sdk::HostDirectoryOpts;

#[tokio::main]
async fn main() -> eyre::Result<()> {
    dagger_sdk::connect(|client| async move {
        let host_source_dir = client.host().directory_opts(
            "examples/test-the-application/app",
            HostDirectoryOpts {
                exclude: Some(vec!["node_modules", "ci/"]),
                include: None,
                no_cache: None,
            },
        );

        let source = client
            .container()
            .from("node:16")
            .with_mounted_directory("/src", host_source_dir);

        let runner = source
            .with_workdir("/src")
            .with_exec(vec!["npm", "install"]);

        let out = runner
            .with_exec(vec!["npm", "test", "--", "--watchAll=false"])
            .stderr()
            .await?;

        println!("{}", out);

        Ok(())
    })
    .await?;

    Ok(())
}
