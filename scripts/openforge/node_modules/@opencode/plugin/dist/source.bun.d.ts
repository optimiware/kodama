export declare function prepareSource(entrypoint: string, track: (file: string, directory?: boolean) => void): Promise<{
    version: string;
    load: () => Promise<unknown>;
}>;
