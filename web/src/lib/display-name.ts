export function displayName(value?:string){const name=(value??"").trim();return (name.includes("@")?name.slice(0,name.lastIndexOf("@")):name)||"Someone";}
