"use client";
import {OrganizationPeople} from "./organization-people";
export function WrenMembersPanel({workspaceId}:{workspaceId:string}){
 return <OrganizationPeople workspaceId={workspaceId} description="Organization members can see shared channels. Private groups keep their own membership."/>;
}
