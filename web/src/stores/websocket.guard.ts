/*
 * Generated type guards for "websocket.ts".
 * WARNING: Do not manually change this file.
 */
import { Event, TagEvent, ConverterEvent, PcapStatsEvent, ConfigEvent, WebhooksEvent, PcapOverIPEndpointsEvent } from "./websocket";
import { isConfig } from "../apiClient.guard";

export function isEvent(obj: unknown): obj is Event {
    const typedObj = obj as Event
    return (
        (typedObj !== null &&
            typeof typedObj === "object" ||
            typeof typedObj === "function") &&
        typeof typedObj["Type"] === "string"
    )
}

export function isTagEvent(obj: unknown): obj is TagEvent {
    const typedObj = obj as TagEvent
    return (
        (typedObj !== null &&
            typeof typedObj === "object" ||
            typeof typedObj === "function") &&
        (typedObj["Type"] === "tagAdded" ||
            typedObj["Type"] === "tagDeleted" ||
            typedObj["Type"] === "tagUpdated" ||
            typedObj["Type"] === "tagEvaluated") &&
        (typedObj["Tag"] !== null &&
            typeof typedObj["Tag"] === "object" ||
            typeof typedObj["Tag"] === "function") &&
        typeof typedObj["Tag"]["Name"] === "string" &&
        typeof typedObj["Tag"]["Definition"] === "string" &&
        typeof typedObj["Tag"]["Color"] === "string" &&
        typeof typedObj["Tag"]["MatchingCount"] === "number" &&
        typeof typedObj["Tag"]["UncertainCount"] === "number" &&
        typeof typedObj["Tag"]["Referenced"] === "boolean" &&
        Array.isArray(typedObj["Tag"]["Converters"]) &&
        typedObj["Tag"]["Converters"].every((e: any) =>
            typeof e === "string"
        )
    )
}

export function isConverterEvent(obj: unknown): obj is ConverterEvent {
    const typedObj = obj as ConverterEvent
    return (
        (typedObj !== null &&
            typeof typedObj === "object" ||
            typeof typedObj === "function") &&
        (typedObj["Type"] === "converterCompleted" ||
            typedObj["Type"] === "converterDeleted" ||
            typedObj["Type"] === "converterAdded" ||
            typedObj["Type"] === "converterRestarted") &&
        (typedObj["Converter"] !== null &&
            typeof typedObj["Converter"] === "object" ||
            typeof typedObj["Converter"] === "function") &&
        typeof typedObj["Converter"]["Name"] === "string" &&
        typeof typedObj["Converter"]["CachedStreamCount"] === "number" &&
        Array.isArray(typedObj["Converter"]["Processes"]) &&
        typedObj["Converter"]["Processes"].every((e: any) =>
            (e !== null &&
                typeof e === "object" ||
                typeof e === "function") &&
            typeof e["Running"] === "boolean" &&
            typeof e["ExitCode"] === "number" &&
            typeof e["Pid"] === "number" &&
            typeof e["Errors"] === "number"
        )
    )
}

export function isPcapStatsEvent(obj: unknown): obj is PcapStatsEvent {
    const typedObj = obj as PcapStatsEvent
    return (
        (typedObj !== null &&
            typeof typedObj === "object" ||
            typeof typedObj === "function") &&
        (typedObj["Type"] === "indexesMerged" ||
            typedObj["Type"] === "pcapProcessed") &&
        (typedObj["PcapStats"] !== null &&
            typeof typedObj["PcapStats"] === "object" ||
            typeof typedObj["PcapStats"] === "function") &&
        typeof typedObj["PcapStats"]["PcapCount"] === "number" &&
        typeof typedObj["PcapStats"]["PacketCount"] === "number" &&
        typeof typedObj["PcapStats"]["ImportJobCount"] === "number" &&
        typeof typedObj["PcapStats"]["IndexCount"] === "number" &&
        typeof typedObj["PcapStats"]["StreamCount"] === "number" &&
        typeof typedObj["PcapStats"]["StreamRecordCount"] === "number" &&
        typeof typedObj["PcapStats"]["PacketRecordCount"] === "number"
    )
}

export function isConfigEvent(obj: unknown): obj is ConfigEvent {
    const typedObj = obj as ConfigEvent
    return (
        (typedObj !== null &&
            typeof typedObj === "object" ||
            typeof typedObj === "function") &&
        typedObj["Type"] === "configUpdated" &&
        isConfig(typedObj["Config"]) as boolean
    )
}

export function isWebhooksEvent(obj: unknown): obj is WebhooksEvent {
    const typedObj = obj as WebhooksEvent
    return (
        (typedObj !== null &&
            typeof typedObj === "object" ||
            typeof typedObj === "function") &&
        typedObj["Type"] === "webhooksUpdated" &&
        Array.isArray(typedObj["Webhooks"]) &&
        typedObj["Webhooks"].every((e: any) =>
            typeof e === "string"
        )
    )
}

export function isPcapOverIPEndpointsEvent(obj: unknown): obj is PcapOverIPEndpointsEvent {
    const typedObj = obj as PcapOverIPEndpointsEvent
    return (
        (typedObj !== null &&
            typeof typedObj === "object" ||
            typeof typedObj === "function") &&
        typedObj["Type"] === "pcapOverIPEndpointsUpdated" &&
        Array.isArray(typedObj["PcapOverIPEndpoints"]) &&
        typedObj["PcapOverIPEndpoints"].every((e: any) =>
            (e !== null &&
                typeof e === "object" ||
                typeof e === "function") &&
            typeof e["Address"] === "string" &&
            typeof e["LastConnected"] === "number" &&
            typeof e["LastDisconnected"] === "number" &&
            typeof e["ReceivedPackets"] === "number"
        )
    )
}
