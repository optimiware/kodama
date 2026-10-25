/**
 * Named HTTP status codes for the unstable HTTP modules.
 *
 * This module centralizes the mapping from literal names of the known HTTP
 * status codes, such as `"OK"` and `"Conflict"`, to their numeric codes. Use
 * {@link fromLiteral} to obtain a status code from a literal name instead of
 * remembering raw numbers.
 *
 * @stability unstable
 * @since 4.0.0
 */
declare const codeByLiteral: {
    readonly Continue: 100;
    readonly SwitchingProtocols: 101;
    readonly Processing: 102;
    readonly EarlyHints: 103;
    readonly OK: 200;
    readonly Ok: 200;
    readonly Created: 201;
    readonly Accepted: 202;
    readonly NonAuthoritativeInformation: 203;
    readonly NoContent: 204;
    readonly ResetContent: 205;
    readonly PartialContent: 206;
    readonly MultiStatus: 207;
    readonly AlreadyReported: 208;
    readonly ImUsed: 226;
    readonly MultipleChoices: 300;
    readonly MovedPermanently: 301;
    readonly Found: 302;
    readonly SeeOther: 303;
    readonly NotModified: 304;
    readonly TemporaryRedirect: 307;
    readonly PermanentRedirect: 308;
    readonly BadRequest: 400;
    readonly Unauthorized: 401;
    readonly PaymentRequired: 402;
    readonly Forbidden: 403;
    readonly NotFound: 404;
    readonly MethodNotAllowed: 405;
    readonly NotAcceptable: 406;
    readonly ProxyAuthenticationRequired: 407;
    readonly RequestTimeout: 408;
    readonly Conflict: 409;
    readonly Gone: 410;
    readonly LengthRequired: 411;
    readonly PreconditionFailed: 412;
    readonly PayloadTooLarge: 413;
    readonly UriTooLong: 414;
    readonly UnsupportedMediaType: 415;
    readonly RangeNotSatisfiable: 416;
    readonly ExpectationFailed: 417;
    readonly ImATeapot: 418;
    readonly MisdirectedRequest: 421;
    readonly UnprocessableEntity: 422;
    readonly Locked: 423;
    readonly FailedDependency: 424;
    readonly TooEarly: 425;
    readonly UpgradeRequired: 426;
    readonly PreconditionRequired: 428;
    readonly TooManyRequests: 429;
    readonly RequestHeaderFieldsTooLarge: 431;
    readonly UnavailableForLegalReasons: 451;
    readonly InternalServerError: 500;
    readonly NotImplemented: 501;
    readonly BadGateway: 502;
    readonly ServiceUnavailable: 503;
    readonly GatewayTimeout: 504;
    readonly HttpVersionNotSupported: 505;
    readonly VariantAlsoNegotiates: 506;
    readonly InsufficientStorage: 507;
    readonly LoopDetected: 508;
    readonly NotExtended: 510;
    readonly NetworkAuthenticationRequired: 511;
};
/**
 * Union of literal names for the known HTTP status codes.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Literal = keyof typeof codeByLiteral;
/**
 * Returns the numeric HTTP status code for a literal name.
 *
 * **Example** (Obtaining status codes from literal names)
 *
 * ```ts import.meta.vitest
 * import { HttpStatus } from "effect/http"
 *
 * HttpStatus.fromLiteral("OK") // => 200
 * HttpStatus.fromLiteral("Conflict") // => 409
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const fromLiteral: <L extends Literal>(literal: L) => (typeof codeByLiteral)[L];
export {};
//# sourceMappingURL=HttpStatus.d.ts.map